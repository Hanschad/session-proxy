package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// DebugMode enables verbose logging
var DebugMode bool

func debugLog(format string, args ...interface{}) {
	if DebugMode {
		log.Printf("[DEBUG] "+format, args...)
	}
}

const maxTextPayloadLogBytes = 256

const (
	// AWS SSM Agent uses 1024-byte stream payloads (agent/session/config.StreamDataPayloadSize).
	// Larger payloads have proven unstable under load for SSH-over-SSM proxying.
	defaultStreamChunkSize = 1024
	minStreamChunkSize     = 256
	maxStreamChunkSize     = 1024

	// To keep debug mode usable during large transfers, sample per-frame logs.
	rxFrameLogEveryN = 1000
)

const (
	// Mirrors amazon-ssm-agent defaults.
	defaultRetransmissionTimeout = 200 * time.Millisecond
	maxRetransmissionTimeout     = 1 * time.Second
	resendSleepInterval          = 100 * time.Millisecond

	// maxResendAttempts bounds how many times we retransmit the oldest unacked message.
	// After this many attempts without ACK, the transport is considered dead.
	// At ~1s max RTO this gives ~60s of retransmission before giving up.
	maxResendAttempts = 60

	// maxPauseTime bounds how long the adapter stays in pause_publication state.
	// If the remote side pauses us for longer than this, the transport is considered dead.
	maxPauseTime = 45 * time.Second

	// Upper bound on buffered, unacknowledged outgoing data.
	// For a TCP proxy, dropping is not acceptable; we will backpressure writes if this is exceeded.
	// A conservative default send window to avoid MGS/server-side channel closures under sustained upload.
	// Can be overridden via SESSION_PROXY_SSM_MAX_UNACKED_BYTES.
	defaultMaxOutgoingUnackedBytes = 256 * 1024 // 256KB
	minMaxOutgoingUnackedBytes     = 64 * 1024  // 64KB
	maxMaxOutgoingUnackedBytes     = 1024 * 1024 * 1024
)

type outgoingMessage struct {
	msgID       uuid.UUID
	data        []byte
	lastSent    time.Time
	resendCount int
}

func looksMostlyText(b []byte) bool {
	// Heuristic: allow printable ASCII plus common whitespace.
	// This avoids dumping binary TLS payloads while still logging useful text like SSH banners.
	if len(b) == 0 {
		return true
	}
	printable := 0
	for _, c := range b {
		switch {
		case c == '\r' || c == '\n' || c == '\t':
			printable++
		case c >= 0x20 && c <= 0x7e:
			printable++
		}
	}
	return printable*100/len(b) >= 90
}

type timeoutError struct{}

func (e timeoutError) Error() string   { return "i/o timeout" }
func (e timeoutError) Timeout() bool   { return true }
func (e timeoutError) Temporary() bool { return true }

var (
	errAdapterClosedBeforeHandshake = errors.New("adapter closed before handshake completed")
	errChannelClosedByRemote        = errors.New("channel closed by remote")
	errPausePublicationTimedOut     = errors.New("pause_publication exceeded limit")
	errResendAttemptsExceeded       = errors.New("retransmission attempts exceeded limit")
	errResumeBudgetExhausted        = errors.New("websocket resume budget exhausted")
)

// ResumeFunc obtains fresh data-channel credentials (stream URL + token) for an
// existing SSM session, typically via the ResumeSession API. It is called by the
// adapter when the underlying WebSocket drops and needs to be re-established.
type ResumeFunc func(ctx context.Context) (streamUrl, token string, err error)

// AdapterOption customizes adapter construction.
type AdapterOption func(*Adapter)

// WithResume enables transparent WebSocket reconnection: when the transport
// drops, the adapter calls fn to get a fresh stream URL/token and swaps in a
// new WebSocket while keeping all session-layer state (sequence numbers,
// unacked buffer, SSH stream) intact.
func WithResume(fn ResumeFunc) AdapterOption {
	return func(a *Adapter) {
		a.resumeFunc = fn
	}
}

const (
	// defaultResumeBudget bounds the total time spent trying to resume a dropped
	// WebSocket before giving up and closing the adapter. Override via
	// SESSION_PROXY_SSM_RESUME_BUDGET.
	defaultResumeBudget = 90 * time.Second
)

// Adapter implements net.Conn over an SSM WebSocket session
type Adapter struct {
	// connMu guards conn and connGen. The generation counter lets goroutines
	// bound to an old WebSocket (readLoop, ping loop) detect that their
	// connection has been replaced and exit without touching the new one.
	connMu  sync.RWMutex
	conn    *websocket.Conn
	connGen uint64

	writeMu sync.Mutex
	seqNum  int64

	// WebSocket resume (ResumeSession-based reconnect) state.
	resumeFunc     ResumeFunc
	resumeDisabled bool // via SESSION_PROXY_SSM_RESUME=off
	resumeBudget   time.Duration

	reconnectMu   sync.Mutex
	reconnecting  bool
	reconnectGate chan struct{} // non-nil while reconnecting; closed on success

	writeDeadlineMu sync.RWMutex
	writeDeadline   time.Time

	chunkSize int

	pauseMu     sync.Mutex
	paused      bool
	pauseCh     chan struct{}
	pausedSince time.Time

	// Outgoing reliability/flow control (mirrors amazon-ssm-agent datachannel behavior)
	outgoingMu              sync.Mutex
	outgoing                map[int64]*outgoingMessage // key: stream seq
	outgoingOldestSeq       int64                      // -1 means empty
	outgoingBytes           int64
	outgoingCond            *sync.Cond
	outgoingClosed          bool // set under outgoingMu by Close(); checked by addOutgoing
	rto                     time.Duration
	maxOutgoingUnackedBytes int64

	rxAckCount uint64 // for log sampling; ACK header seq is always 0

	// Read-side stream used to bridge SSM frames into net.Conn reads.
	// net.Pipe gives us blocking reads plus native deadline support without spawning
	// a goroutine per timed read.
	streamReader net.Conn
	streamWriter net.Conn

	// Handshake state
	handshakeComplete     bool
	handshakeResponded    bool // Track if we already responded to HandshakeRequest
	handshakeDone         chan struct{}
	lastHandshakeResponse *AgentMessage // Saved for retransmission

	// Message deduplication (stores sequence number for age-based eviction)
	seenMsgIds   map[uuid.UUID]int64
	seenMsgIdsMu sync.Mutex

	// Message reordering (per AWS protocol)
	expectedSeqNum    int64                   // Next expected sequence number
	incomingMsgBuffer map[int64]*AgentMessage // Buffer for out-of-order messages
	incomingMsgBufMu  sync.Mutex              // Protects incomingMsgBuffer

	// Lifecycle management
	done          chan struct{}
	closeOnce     sync.Once
	closeReasonMu sync.Mutex
	closeReason   error // first error wins

	clientId string // stable OpenDataChannel client id across resume
}

// ClientVersion is the SSM protocol version reported to AWS SSM service.
// Must match session-manager-plugin version format. Do not change unless protocol changes.
const ClientVersion = "1.2.0.0"

// PingInterval is the interval for sending WebSocket ping frames to keep the connection alive.
// Reduced from 1 minute to 30 seconds to detect half-dead connections faster
// while remaining tolerant of temporary packet loss.
const PingInterval = 30 * time.Second

const (
	// pongWait is how long we allow the peer to be silent (no pongs) before treating the
	// connection as dead. This is critical to avoid half-open connections that hang reads.
	// At 30s ping interval, this gives ~70s tolerance (miss 2 pings + margin).
	pongWait = 2*PingInterval + 10*time.Second
	// writeWait is the max time allowed for a single WebSocket write.
	writeWait = 10 * time.Second
	// dialTimeout bounds TCP connect time for the WebSocket.
	dialTimeout = 30 * time.Second
	// tcpKeepAlive requests OS-level keepalives on the underlying TCP connection.
	tcpKeepAlive = 30 * time.Second
)

func NewAdapter(ctx context.Context, streamUrl, token string, opts ...AdapterOption) (*Adapter, error) {
	debugLog("Dialing WebSocket: %s", streamUrl)
	wsConn, err := adapterDialWebSocketHook(ctx, streamUrl)
	if err != nil {
		return nil, fmt.Errorf("websocket dial failed: %w", err)
	}

	readConn, writeConn := net.Pipe()
	adapter := &Adapter{
		conn:              wsConn,
		connGen:           1,
		streamReader:      readConn,
		streamWriter:      writeConn,
		seenMsgIds:        make(map[uuid.UUID]int64),
		handshakeDone:     make(chan struct{}),
		done:              make(chan struct{}),
		incomingMsgBuffer: make(map[int64]*AgentMessage),
		expectedSeqNum:    0,
		chunkSize:         defaultStreamChunkSize,
		clientId:          CleanUUID(uuid.New()),

		outgoing:                make(map[int64]*outgoingMessage),
		outgoingOldestSeq:       -1,
		rto:                     defaultRetransmissionTimeout,
		maxOutgoingUnackedBytes: defaultMaxOutgoingUnackedBytes,
	}
	adapter.outgoingCond = sync.NewCond(&adapter.outgoingMu)
	applyAdapterOptions(adapter, opts)

	// Allow overriding stream chunk size via env var for performance tuning.
	// This impacts client->agent throughput for large uploads.
	if v := os.Getenv("SESSION_PROXY_SSM_CHUNK_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n >= minStreamChunkSize && n <= maxStreamChunkSize {
				adapter.chunkSize = n
			} else {
				log.Printf("[WARN] SESSION_PROXY_SSM_CHUNK_SIZE=%q out of range (%d..%d), using default %d", v, minStreamChunkSize, maxStreamChunkSize, defaultStreamChunkSize)
			}
		} else {
			log.Printf("[WARN] SESSION_PROXY_SSM_CHUNK_SIZE=%q invalid, using default %d", v, defaultStreamChunkSize)
		}
	}

	// Allow overriding max unacknowledged outgoing bytes. This effectively caps the send window.
	// Useful when MGS flow-control is aggressive and we want to avoid overshooting its internal buffers.
	if v := os.Getenv("SESSION_PROXY_SSM_MAX_UNACKED_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			if n >= minMaxOutgoingUnackedBytes && n <= maxMaxOutgoingUnackedBytes {
				adapter.maxOutgoingUnackedBytes = n
			} else {
				log.Printf("[WARN] SESSION_PROXY_SSM_MAX_UNACKED_BYTES=%q out of range (%d..%d), using default %d", v, minMaxOutgoingUnackedBytes, maxMaxOutgoingUnackedBytes, defaultMaxOutgoingUnackedBytes)
			}
		} else {
			log.Printf("[WARN] SESSION_PROXY_SSM_MAX_UNACKED_BYTES=%q invalid, using default %d", v, defaultMaxOutgoingUnackedBytes)
		}
	}

	if err := adapter.sendOpenDataChannel(wsConn, token); err != nil {
		wsConn.Close()
		return nil, fmt.Errorf("failed to send OpenDataChannel message: %w", err)
	}

	adapter.installPongHandler(wsConn, adapter.connGen)

	go adapter.readLoopForGen(adapter.connGen)
	go adapter.pingLoopForGen(adapter.connGen)
	go adapter.resendLoop()

	return adapter, nil
}

// dispatchMessage routes message to appropriate handler. Returns false if channel closed.
func (a *Adapter) dispatchMessage(msg *AgentMessage) bool {
	isDuplicate := a.markMessageSeen(msg.Header.MessageId, msg.Header.SequenceNumber)

	switch msg.Header.MessageType {
	case MsgTypeOutputStreamData:
		a.handleOutputStream(msg, isDuplicate)

	case MsgTypeAcknowledge:
		a.handleAcknowledge(msg)

	case MsgTypePausePublication:
		a.pausePublication()
		debugLog("Processed pause_publication")

	case MsgTypeStartPublication:
		a.resumePublication()
		debugLog("Processed start_publication")

	case MsgTypeChannelClosed:
		// This is an important signal from the remote side; log payload to aid debugging.
		if len(msg.Payload) > 0 && len(msg.Payload) <= 4*1024 && looksMostlyText(msg.Payload) {
			log.Printf("[WARN] channel_closed by remote: %s", string(msg.Payload))
		} else {
			log.Printf("[WARN] channel_closed by remote (payload_len=%d)", len(msg.Payload))
		}
		if a.streamWriter != nil {
			_ = a.streamWriter.Close()
		}
		a.closeWithError(errChannelClosedByRemote)
		return false

	default:
		debugLog("Ignored Message Type: %s", msg.Header.MessageType)
	}
	return true
}

func (a *Adapter) handleAcknowledge(msg *AgentMessage) {
	// ACK payload is JSON describing which stream message was received.
	var ack AcknowledgeContent
	if err := json.Unmarshal(msg.Payload, &ack); err != nil {
		debugLog("ACK unmarshal error: %v", err)
		// Still resume on any ACK per contract semantics.
		a.resumePublication()
		return
	}

	// Resume is part of flow-control semantics, but we only mark a message as ACKed if it matches our outgoing stream.
	if ack.MessageType != MsgTypeInputStreamData {
		a.resumePublication()
		return
	}

	ackID, err := uuid.Parse(ack.MessageId)
	if err != nil {
		debugLog("ACK invalid message id %q: %v", ack.MessageId, err)
		a.resumePublication()
		return
	}

	a.ackOutgoing(ack.SequenceNumber, ackID)
	// Always resume after an ACK (matches amazon-ssm-agent contract comment).
	a.resumePublication()
}

func (a *Adapter) ackOutgoing(seq int64, msgID uuid.UUID) {
	a.outgoingMu.Lock()
	defer a.outgoingMu.Unlock()

	om, ok := a.outgoing[seq]
	if !ok {
		return
	}
	if om.msgID != msgID {
		// Ignore mismatched ACKs; this should not normally happen but avoids corrupting the stream.
		debugLog("ACK mismatch: seq=%d got=%s want=%s", seq, msgID.String(), om.msgID.String())
		return
	}

	// Update retransmission timeout based on observed RTT.
	if !om.lastSent.IsZero() {
		rtt := time.Since(om.lastSent)
		if rtt > 0 {
			newRTO := rtt * 2
			if newRTO < defaultRetransmissionTimeout {
				newRTO = defaultRetransmissionTimeout
			}
			if newRTO > maxRetransmissionTimeout {
				newRTO = maxRetransmissionTimeout
			}
			a.rto = newRTO
		}
	}

	delete(a.outgoing, seq)
	a.outgoingBytes -= int64(len(om.data))

	// Advance oldest pointer (fast path for in-order ACKs).
	if len(a.outgoing) == 0 {
		a.outgoingOldestSeq = -1
	} else if seq == a.outgoingOldestSeq {
		// Try a small sequential scan first (typical ACK order).
		const maxScan = 1024
		for i := 0; i < maxScan; i++ {
			a.outgoingOldestSeq++
			if _, ok := a.outgoing[a.outgoingOldestSeq]; ok {
				break
			}
		}
		if _, ok := a.outgoing[a.outgoingOldestSeq]; !ok {
			// Fallback: find the true minimum if we hit a gap.
			min := int64(1<<63 - 1)
			for k := range a.outgoing {
				if k < min {
					min = k
				}
			}
			a.outgoingOldestSeq = min
		}
	}

	// Wake any writers blocked on buffer limits.
	if a.outgoingCond != nil {
		a.outgoingCond.Broadcast()
	}
}

func (a *Adapter) addOutgoing(seq int64, msgID uuid.UUID, data []byte) error {
	// Enforce a bounded amount of unacknowledged data; otherwise a burst can OOM.
	need := int64(len(data))

	a.outgoingMu.Lock()
	defer a.outgoingMu.Unlock()

	if a.outgoingCond == nil {
		a.outgoingCond = sync.NewCond(&a.outgoingMu)
	}

	var deadlineTimer *time.Timer
	defer func() {
		if deadlineTimer != nil {
			deadlineTimer.Stop()
		}
	}()

	logged := false
	for a.outgoingBytes+need > a.maxOutgoingUnackedBytes {
		if a.outgoingClosed {
			return io.ErrClosedPipe
		}
		if !logged {
			debugLog("Outgoing buffer full (bytes=%d need=%d limit=%d), backpressuring", a.outgoingBytes, need, a.maxOutgoingUnackedBytes)
			logged = true
		}
		if err := a.ensureWriteDeadlineWake(&deadlineTimer, func() {
			a.outgoingMu.Lock()
			if a.outgoingCond != nil {
				a.outgoingCond.Broadcast()
			}
			a.outgoingMu.Unlock()
		}); err != nil {
			return err
		}
		a.outgoingCond.Wait()
	}

	if a.outgoingClosed {
		return io.ErrClosedPipe
	}

	a.outgoing[seq] = &outgoingMessage{msgID: msgID, data: data, lastSent: time.Now()}
	a.outgoingBytes += need
	if a.outgoingOldestSeq == -1 || seq < a.outgoingOldestSeq {
		a.outgoingOldestSeq = seq
	}
	return nil
}

func (a *Adapter) dropOutgoing(seq int64, msgID uuid.UUID) {
	a.outgoingMu.Lock()
	defer a.outgoingMu.Unlock()

	om, ok := a.outgoing[seq]
	if !ok {
		return
	}
	if om.msgID != msgID {
		return
	}

	delete(a.outgoing, seq)
	a.outgoingBytes -= int64(len(om.data))

	if len(a.outgoing) == 0 {
		a.outgoingOldestSeq = -1
	} else if seq == a.outgoingOldestSeq {
		min := int64(1<<63 - 1)
		for k := range a.outgoing {
			if k < min {
				min = k
			}
		}
		a.outgoingOldestSeq = min
	}

	if a.outgoingCond != nil {
		a.outgoingCond.Broadcast()
	}
}

func (a *Adapter) resendLoop() {
	ticker := time.NewTicker(resendSleepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.done:
			return
		case <-ticker.C:
		}

		if a.Reconnecting() {
			continue
		}

		// Check pause timeout independently of resend logic.
		a.pauseMu.Lock()
		paused := a.paused
		pausedSince := a.pausedSince
		a.pauseMu.Unlock()
		if paused {
			if !pausedSince.IsZero() && time.Since(pausedSince) > maxPauseTime {
				log.Printf("[WARN] publication paused for %s (limit=%s), closing adapter", time.Since(pausedSince), maxPauseTime)
				a.closeWithError(errPausePublicationTimedOut)
				return
			}
			continue
		}

		// Only resend the oldest unacknowledged message, matching amazon-ssm-agent behavior.
		var (
			data []byte
			seq  int64
			rto  time.Duration
		)

		a.outgoingMu.Lock()
		if a.outgoingOldestSeq == -1 || len(a.outgoing) == 0 {
			a.outgoingMu.Unlock()
			continue
		}
		seq = a.outgoingOldestSeq
		om := a.outgoing[seq]
		if om == nil {
			// Inconsistent pointer; recompute next loop.
			min := int64(1<<63 - 1)
			for k := range a.outgoing {
				if k < min {
					min = k
				}
			}
			a.outgoingOldestSeq = min
			a.outgoingMu.Unlock()
			continue
		}
		rto = a.rto
		if rto <= 0 {
			rto = defaultRetransmissionTimeout
		}
		// After the first resend with no ACK, back off to maxRetransmissionTimeout
		// so we don't exhaust the attempt budget too quickly on no-ACK paths.
		if om.resendCount > 0 && rto < maxRetransmissionTimeout {
			rto = maxRetransmissionTimeout
		}

		if time.Since(om.lastSent) <= rto {
			a.outgoingMu.Unlock()
			continue
		}

		om.resendCount++
		if om.resendCount > maxResendAttempts {
			a.outgoingMu.Unlock()
			log.Printf("[WARN] oldest unacked message (seq=%d) exceeded max resend attempts (%d), closing adapter", seq, maxResendAttempts)
			a.closeWithError(errResendAttemptsExceeded)
			return
		}

		// Mark resent before sending to avoid tight loops on very small timeouts.
		om.lastSent = time.Now()
		data = om.data
		a.outgoingMu.Unlock()

		if err := a.writeRaw(data, MsgTypeInputStreamData, PayloadTypeOutput); err != nil {
			debugLog("Resend failed: seq=%d err=%v", seq, err)
			if a.handleTransportFailure(fmt.Errorf("resend write failed: %w", err)) {
				return
			}
			return
		}
	}
}

// markMessageSeen checks and marks a message ID as seen. Returns true if duplicate.
// Evicts oldest entry (lowest sequence number) when map exceeds maxSeenMsgIds.
const maxSeenMsgIds = 1000

func (a *Adapter) markMessageSeen(id uuid.UUID, seq int64) bool {
	a.seenMsgIdsMu.Lock()
	defer a.seenMsgIdsMu.Unlock()

	if _, exists := a.seenMsgIds[id]; exists {
		return true
	}

	// Evict oldest message (lowest sequence number)
	if len(a.seenMsgIds) >= maxSeenMsgIds {
		var oldestId uuid.UUID
		var oldestSeq int64 = 1<<63 - 1 // math.MaxInt64
		for k, v := range a.seenMsgIds {
			if v < oldestSeq {
				oldestSeq = v
				oldestId = k
			}
		}
		delete(a.seenMsgIds, oldestId)
	}

	a.seenMsgIds[id] = seq
	return false
}

// handleOutputStream processes output_stream_data messages based on PayloadType
func (a *Adapter) handleOutputStream(msg *AgentMessage, isDuplicate bool) {
	switch msg.Header.PayloadType {
	case PayloadTypeHandshakeRequest:
		a.handleHandshakeRequestPayload(msg, isDuplicate)
	case PayloadTypeHandshakeComplete:
		a.handleHandshakeCompletePayload(msg)
	default:
		a.handleDataMessage(msg)
	}
}

// handleHandshakeRequestPayload handles HandshakeRequest payload type
func (a *Adapter) handleHandshakeRequestPayload(msg *AgentMessage, isDuplicate bool) {
	debugLog("Received HandshakeRequest: %s", string(msg.Payload))

	if isDuplicate || a.handshakeResponded {
		debugLog("Received duplicate HandshakeRequest, resending ACK + Response")
		if err := a.sendAck(msg); err != nil {
			debugLog("Ack Send Error: %v", err)
			a.closeWithError(fmt.Errorf("send duplicate handshake ack: %w", err))
			return
		}
		if err := a.resendHandshakeResponse(); err != nil {
			debugLog("HandshakeResponse resend error: %v", err)
			a.closeWithError(fmt.Errorf("resend handshake response: %w", err))
		}
		return
	}

	if err := a.handleHandshakeRequest(msg); err != nil {
		debugLog("HandshakeRequest handling error: %v", err)
		a.closeWithError(fmt.Errorf("handle handshake request: %w", err))
		return
	}

	a.updateExpectedSeqNum(msg, "HandshakeRequest")
}

// handleHandshakeCompletePayload handles HandshakeComplete payload type
func (a *Adapter) handleHandshakeCompletePayload(msg *AgentMessage) {
	debugLog("Received HandshakeComplete: %s", string(msg.Payload))

	if !a.handshakeComplete {
		a.handshakeComplete = true
		close(a.handshakeDone)
		a.updateExpectedSeqNum(msg, "HandshakeComplete")
	}

	if err := a.sendAck(msg); err != nil {
		debugLog("Ack Send Error: %v", err)
		a.closeWithError(fmt.Errorf("send handshake complete ack: %w", err))
	}
}

// updateExpectedSeqNum updates the expected sequence number after processing a message
func (a *Adapter) updateExpectedSeqNum(msg *AgentMessage, context string) {
	a.incomingMsgBufMu.Lock()
	a.expectedSeqNum = msg.Header.SequenceNumber + 1
	a.incomingMsgBufMu.Unlock()
	debugLog("Updated expectedSeqNum to %d after %s", a.expectedSeqNum, context)
}

func (a *Adapter) handleHandshakeRequest(orig *AgentMessage) error {
	// Per SSM protocol (from session-manager-plugin streaming.go line 619-631):
	// 1. First send ACK for the HandshakeRequest
	// 2. Then send HandshakeResponse
	// Flow: HandshakeRequest -> ACK -> HandshakeResponse -> ACK -> HandshakeComplete -> ACK

	// Mark that we're responding to handshake
	a.handshakeResponded = true

	// Step 1: Send ACK for HandshakeRequest
	if err := a.sendAck(orig); err != nil {
		debugLog("Failed to send ACK for HandshakeRequest: %v", err)
		return err
	}

	// Step 2: Build and send HandshakeResponse
	// For port forwarding (SSH), we just accept the SessionType
	actions := []ProcessedClientAction{
		{
			ActionType:   "SessionType",
			ActionStatus: 1, // Success
			// ActionResult empty
		},
	}

	responseMsg, err := NewHandshakeResponseMessage(a.nextSeq(), ClientVersion, actions)
	if err != nil {
		debugLog("Failed to build HandshakeResponse: %v", err)
		return err
	}

	debugLog("TX HandshakeResponse: %s", string(responseMsg.Payload))

	// Debug: show full binary output
	respBytes, _ := responseMsg.MarshalBinary()
	debugLog("TX HandshakeResponse binary: total=%d bytes, header: HL=%d MsgType=%s SchemaVer=%d Seq=%d Flags=%d PayloadType=%d PayloadLen=%d",
		len(respBytes),
		responseMsg.Header.HeaderLength,
		responseMsg.Header.MessageType,
		responseMsg.Header.SchemaVersion,
		responseMsg.Header.SequenceNumber,
		responseMsg.Header.Flags,
		responseMsg.Header.PayloadType,
		responseMsg.Header.PayloadLength)
	if len(respBytes) > 20 {
		debugLog("TX HandshakeResponse first 40 bytes: %x", respBytes[:min(40, len(respBytes))])
	}

	// Save for retransmission (in case of duplicate HandshakeRequest)
	a.lastHandshakeResponse = responseMsg

	return a.writeMessage(responseMsg)
}

// resendHandshakeResponse resends the saved HandshakeResponse
func (a *Adapter) resendHandshakeResponse() error {
	if a.lastHandshakeResponse == nil {
		debugLog("No saved HandshakeResponse to resend")
		return nil
	}

	debugLog("TX HandshakeResponse (resend)")
	return a.writeMessage(a.lastHandshakeResponse)
}

func (a *Adapter) sendAck(orig *AgentMessage) error {
	ack, err := NewAcknowledgeMessage(orig.Header.MessageType, orig.Header.MessageId, orig.Header.SequenceNumber)
	if err != nil {
		return err
	}

	debugLog("TX ACK for MsgId=%s Seq=%d", orig.Header.MessageId.String(), orig.Header.SequenceNumber)
	return a.writeMessage(ack)
}

func (a *Adapter) writeMessage(msg *AgentMessage) error {
	data, err := msg.MarshalBinary()
	if err != nil {
		debugLog("writeMessage MarshalBinary error: %v", err)
		return err
	}
	return a.writeRaw(data, msg.Header.MessageType, msg.Header.PayloadType)
}

func (a *Adapter) writeRaw(data []byte, msgType string, payloadType uint32) error {
	if err := a.waitUntilConnected(); err != nil {
		return err
	}

	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	conn, gen := a.getConn()
	if conn == nil {
		return io.ErrClosedPipe
	}

	// SSM uses BinaryMessage for frames
	_ = conn.SetWriteDeadline(a.effectiveWriteDeadline())
	err := conn.WriteMessage(websocket.BinaryMessage, data)
	if err != nil {
		debugLog("WebSocket WriteMessage FAILED: gen=%d err=%v", gen, err)
		if a.handleTransportFailure(err) {
			return err
		}
		return err
	}

	// Logging every data frame is extremely noisy (and can become a bottleneck).
	// Keep success logs for control frames, but always log failures above.
	if msgType != MsgTypeAcknowledge && (msgType != MsgTypeInputStreamData || payloadType != PayloadTypeOutput) {
		debugLog("WebSocket WriteMessage OK: %d bytes, MsgType=%q PayloadType=%d", len(data), msgType, payloadType)
	}
	return nil
}

func (a *Adapter) nextSeq() int64 {
	// Adapter.Write may be called concurrently (net.Conn supports concurrent use),
	// so sequence assignment must be atomic.
	return atomic.AddInt64(&a.seqNum, 1) - 1
}

// WaitForHandshake blocks until the SSM handshake is complete, adapter closes, or context is cancelled.
func (a *Adapter) WaitForHandshake(ctx context.Context) error {
	select {
	case <-a.handshakeDone:
		debugLog("Handshake completed, ready for data transfer")
		return nil
	default:
	}

	select {
	case <-a.handshakeDone:
		debugLog("Handshake completed, ready for data transfer")
		return nil
	case <-a.done:
		select {
		case <-a.handshakeDone:
			debugLog("Handshake completed before shutdown was observed")
			return nil
		default:
		}
		if err := a.CloseReason(); err != nil {
			return fmt.Errorf("%w: %w", errAdapterClosedBeforeHandshake, err)
		}
		return errAdapterClosedBeforeHandshake
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Read implements net.Conn.Read
func (a *Adapter) Read(b []byte) (n int, err error) {
	if a.streamReader == nil {
		return 0, io.ErrClosedPipe
	}
	n, err = a.streamReader.Read(b)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return n, timeoutError{}
	}
	return n, err
}

// Write implements net.Conn.Write
func (a *Adapter) Write(b []byte) (n int, err error) {
	// In debug mode, logging per Write() (and per chunk) can easily become the bottleneck.
	if len(b) <= maxTextPayloadLogBytes && looksMostlyText(b) {
		debugLog("TX SSH Data (text): %q", string(b))
	}

	chunkSize := a.chunkSize
	if chunkSize <= 0 {
		chunkSize = defaultStreamChunkSize
	}
	totalWritten := 0

	for len(b) > 0 {
		if err := a.waitForPublication(); err != nil {
			return totalWritten, err
		}

		sendLen := len(b)
		if sendLen > chunkSize {
			sendLen = chunkSize
		}

		// Copy payload since the caller may reuse b after Write returns.
		payload := make([]byte, sendLen)
		copy(payload, b[:sendLen])

		seq := a.nextSeq()
		msg, err := NewInputMessage(payload, seq)
		if err != nil {
			return totalWritten, err
		}

		data, err := msg.MarshalBinary()
		if err != nil {
			return totalWritten, err
		}

		// Track before sending so we can't miss a fast ACK.
		if err := a.addOutgoing(seq, msg.Header.MessageId, data); err != nil {
			return totalWritten, err
		}

		if err := a.writeRaw(data, msg.Header.MessageType, msg.Header.PayloadType); err != nil {
			a.dropOutgoing(seq, msg.Header.MessageId)
			return totalWritten, err
		}

		totalWritten += sendLen
		b = b[sendLen:]
	}

	return totalWritten, nil
}

func (a *Adapter) pausePublication() {
	a.pauseMu.Lock()
	defer a.pauseMu.Unlock()

	if a.paused {
		return
	}
	if a.pauseCh == nil {
		a.pauseCh = make(chan struct{})
	}
	a.paused = true
	a.pausedSince = time.Now()

	// Log state transition only.
	a.outgoingMu.Lock()
	outLen := len(a.outgoing)
	outBytes := a.outgoingBytes
	oldest := a.outgoingOldestSeq
	a.outgoingMu.Unlock()
	debugLog("Publication paused by remote (outgoing=%d bytes=%d oldest=%d)", outLen, outBytes, oldest)
}

func (a *Adapter) resumePublication() {
	a.pauseMu.Lock()
	defer a.pauseMu.Unlock()

	if !a.paused {
		return
	}
	a.paused = false
	if a.pauseCh != nil {
		close(a.pauseCh)
		a.pauseCh = nil
	}

	a.outgoingMu.Lock()
	outLen := len(a.outgoing)
	outBytes := a.outgoingBytes
	oldest := a.outgoingOldestSeq
	a.outgoingMu.Unlock()
	debugLog("Publication resumed (outgoing=%d bytes=%d oldest=%d)", outLen, outBytes, oldest)
}

func (a *Adapter) waitForPublication() error {
	for {
		a.pauseMu.Lock()
		paused := a.paused
		ch := a.pauseCh
		a.pauseMu.Unlock()

		if !paused {
			return nil
		}
		if ch == nil {
			// Should not happen, but avoid a deadlock.
			if err := a.sleepWithWriteDeadline(10 * time.Millisecond); err != nil {
				return err
			}
			continue
		}
		if err := a.waitWithWriteDeadline(ch); err != nil {
			return err
		}
	}
}

// closeWithError records a root-cause error and closes the adapter.
// The first error stored wins; subsequent calls are no-ops for the reason.
func (a *Adapter) closeWithError(err error) {
	a.setCloseReason(err)
	a.Close()
}

// CloseReason returns the root-cause error that triggered adapter shutdown, or nil.
func (a *Adapter) CloseReason() error {
	a.closeReasonMu.Lock()
	defer a.closeReasonMu.Unlock()
	return a.closeReason
}

func (a *Adapter) setCloseReason(err error) {
	if err == nil {
		return
	}

	a.closeReasonMu.Lock()
	if a.closeReason == nil {
		a.closeReason = err
	}
	a.closeReasonMu.Unlock()
}

func (a *Adapter) Close() error {
	a.closeOnce.Do(func() {
		debugLog("Closing Adapter")
		if a.done != nil {
			close(a.done) // Signal that adapter is closed
		}

		// Wake any goroutines blocked on outgoing buffer backpressure.
		a.outgoingMu.Lock()
		a.outgoingClosed = true
		if a.outgoingCond != nil {
			a.outgoingCond.Broadcast()
		}
		a.outgoingMu.Unlock()

		if a.streamReader != nil {
			_ = a.streamReader.Close()
		}
		if a.streamWriter != nil {
			_ = a.streamWriter.Close()
		}
		if a.conn != nil {
			a.connMu.Lock()
			if a.conn != nil {
				_ = a.conn.Close()
				a.conn = nil
			}
			a.connMu.Unlock()
		}
	})
	return nil
}

// startPings is kept for compatibility; ping loops are generation-scoped.
func (a *Adapter) startPings() {
	go a.pingLoopForGen(a.currentConnGen())
}

// Done returns a channel that is closed when the adapter is closed.
// This can be used for lifecycle management.
func (a *Adapter) Done() <-chan struct{} {
	return a.done
}

// LocalAddr implements net.Conn
func (a *Adapter) LocalAddr() net.Addr {
	conn, _ := a.getConn()
	if conn == nil {
		return &net.TCPAddr{}
	}
	return conn.LocalAddr()
}

// RemoteAddr implements net.Conn
func (a *Adapter) RemoteAddr() net.Addr {
	conn, _ := a.getConn()
	if conn == nil {
		return &net.TCPAddr{}
	}
	return conn.RemoteAddr()
}

// SetDeadline implements net.Conn
func (a *Adapter) SetDeadline(t time.Time) error {
	if a.streamReader != nil {
		if err := a.streamReader.SetReadDeadline(t); err != nil {
			return err
		}
	}
	return a.SetWriteDeadline(t)
}

// SetReadDeadline implements net.Conn
func (a *Adapter) SetReadDeadline(t time.Time) error {
	if a.streamReader == nil {
		return io.ErrClosedPipe
	}
	return a.streamReader.SetReadDeadline(t)
}

// SetWriteDeadline implements net.Conn
func (a *Adapter) SetWriteDeadline(t time.Time) error {
	a.writeDeadlineMu.Lock()
	a.writeDeadline = t
	a.writeDeadlineMu.Unlock()

	a.outgoingMu.Lock()
	if a.outgoingCond != nil {
		a.outgoingCond.Broadcast()
	}
	a.outgoingMu.Unlock()

	if a.conn == nil {
		return nil
	}
	conn, _ := a.getConn()
	if conn == nil {
		return nil
	}
	return conn.SetWriteDeadline(t)
}

func (a *Adapter) currentWriteDeadline() time.Time {
	a.writeDeadlineMu.RLock()
	defer a.writeDeadlineMu.RUnlock()
	return a.writeDeadline
}

func (a *Adapter) writeDeadlineExceeded() bool {
	deadline := a.currentWriteDeadline()
	return !deadline.IsZero() && !time.Now().Before(deadline)
}

func (a *Adapter) writeDeadlineWait() (time.Duration, bool) {
	deadline := a.currentWriteDeadline()
	if deadline.IsZero() {
		return 0, false
	}

	wait := time.Until(deadline)
	if wait <= 0 {
		return 0, false
	}

	return wait, true
}

func (a *Adapter) effectiveWriteDeadline() time.Time {
	deadline := a.currentWriteDeadline()
	if deadline.IsZero() {
		return time.Now().Add(writeWait)
	}
	internal := time.Now().Add(writeWait)
	if internal.Before(deadline) {
		return internal
	}
	return deadline
}

func (a *Adapter) ensureWriteDeadlineWake(timer **time.Timer, wake func()) error {
	if a.writeDeadlineExceeded() {
		return timeoutError{}
	}
	if *timer != nil {
		return nil
	}

	wait, ok := a.writeDeadlineWait()
	if !ok {
		if a.writeDeadlineExceeded() {
			return timeoutError{}
		}
		return nil
	}

	*timer = time.AfterFunc(wait, wake)
	return nil
}

func (a *Adapter) sleepWithWriteDeadline(d time.Duration) error {
	if a.writeDeadlineExceeded() {
		return timeoutError{}
	}

	wait := d
	if deadlineWait, ok := a.writeDeadlineWait(); ok {
		wait = minDuration(wait, deadlineWait)
	}
	if wait <= 0 {
		return timeoutError{}
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-timer.C:
		if a.writeDeadlineExceeded() {
			return timeoutError{}
		}
		return nil
	case <-a.done:
		return io.ErrClosedPipe
	}
}

func (a *Adapter) waitWithWriteDeadline(ch <-chan struct{}) error {
	if a.writeDeadlineExceeded() {
		return timeoutError{}
	}

	if wait, ok := a.writeDeadlineWait(); ok {
		timer := time.NewTimer(wait)
		defer timer.Stop()

		select {
		case <-ch:
			return nil
		case <-a.done:
			return io.ErrClosedPipe
		case <-timer.C:
			return timeoutError{}
		}
	}

	select {
	case <-ch:
		return nil
	case <-a.done:
		return io.ErrClosedPipe
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// handleDataMessage processes data messages in sequence order.
// If message is in order, process it and check buffer for next messages.
// If out of order, buffer it for later processing.
func (a *Adapter) handleDataMessage(msg *AgentMessage) {
	seq := msg.Header.SequenceNumber

	a.incomingMsgBufMu.Lock()
	defer a.incomingMsgBufMu.Unlock()

	// Always send ACK (even for out-of-order or duplicate messages)
	if err := a.sendAck(msg); err != nil {
		debugLog("Ack Send Error: %v", err)
		if a.handleTransportFailure(fmt.Errorf("send data ack: %w", err)) {
			return
		}
		return
	}

	if seq == a.expectedSeqNum {
		// Message is in order - process it
		debugLog("Processing in-order message Seq=%d", seq)
		a.processMessage(msg)
		a.expectedSeqNum++

		// Check buffer for subsequent messages
		a.processBufferedMessages()

	} else if seq > a.expectedSeqNum {
		// Out of order - buffer it
		debugLog("Buffering out-of-order message Seq=%d (expected=%d)", seq, a.expectedSeqNum)
		a.incomingMsgBuffer[seq] = msg

	} else {
		// seq < expectedSeqNum - duplicate/old message, already processed
		debugLog("Ignoring old message Seq=%d (expected=%d)", seq, a.expectedSeqNum)
	}
}

// processBufferedMessages processes buffered messages that are now in sequence.
func (a *Adapter) processBufferedMessages() {
	for {
		msg, exists := a.incomingMsgBuffer[a.expectedSeqNum]
		if !exists {
			break
		}

		debugLog("Processing buffered message Seq=%d", a.expectedSeqNum)
		delete(a.incomingMsgBuffer, a.expectedSeqNum)
		a.processMessage(msg)
		a.expectedSeqNum++
	}
}

// processMessage writes the message payload to the pipe.
func (a *Adapter) processMessage(msg *AgentMessage) {
	if a.streamWriter == nil {
		return
	}
	if _, err := a.streamWriter.Write(msg.Payload); err != nil {
		debugLog("Pipe Write Error: %v", err)
	}
}
