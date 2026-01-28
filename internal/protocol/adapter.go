package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
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

	// Upper bound on buffered, unacknowledged outgoing data.
	// For a TCP proxy, dropping is not acceptable; we will backpressure writes if this is exceeded.
	// A conservative default send window to avoid MGS/server-side channel closures under sustained upload.
	// Can be overridden via SESSION_PROXY_SSM_MAX_UNACKED_BYTES.
	defaultMaxOutgoingUnackedBytes = 256 * 1024 // 256KB
	minMaxOutgoingUnackedBytes     = 64 * 1024  // 64KB
	maxMaxOutgoingUnackedBytes     = 1024 * 1024 * 1024
)

type outgoingMessage struct {
	msgID    uuid.UUID
	data     []byte
	lastSent time.Time
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

// Adapter implements net.Conn over an SSM WebSocket session
type Adapter struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	seqNum  int64

	chunkSize int

	pauseMu sync.Mutex
	paused  bool
	pauseCh chan struct{}

	// Outgoing reliability/flow control (mirrors amazon-ssm-agent datachannel behavior)
	outgoingMu              sync.Mutex
	outgoing                map[int64]*outgoingMessage // key: stream seq
	outgoingOldestSeq       int64                      // -1 means empty
	outgoingBytes           int64
	outgoingCond            *sync.Cond
	rto                     time.Duration
	maxOutgoingUnackedBytes int64

	rxAckCount uint64 // for log sampling; ACK header seq is always 0

	// Read-side pipe (replaces complex buffering)
	reader       *io.PipeReader
	writer       *io.PipeWriter
	readDeadline atomic.Value // stores time.Time

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
	done      chan struct{}
	closeOnce sync.Once
}

// ClientVersion is the SSM protocol version reported to AWS SSM service.
// Must match session-manager-plugin version format. Do not change unless protocol changes.
const ClientVersion = "1.2.0.0"

// PingInterval is the interval for sending WebSocket ping frames to keep the connection alive.
const PingInterval = 1 * time.Minute

const (
	// pongWait is how long we allow the peer to be silent (no pongs) before treating the
	// connection as dead. This is critical to avoid half-open connections that hang reads.
	pongWait = 2*PingInterval + 10*time.Second
	// writeWait is the max time allowed for a single WebSocket write.
	writeWait = 10 * time.Second
	// dialTimeout bounds TCP connect time for the WebSocket.
	dialTimeout = 30 * time.Second
	// tcpKeepAlive requests OS-level keepalives on the underlying TCP connection.
	tcpKeepAlive = 30 * time.Second
)

func NewAdapter(ctx context.Context, streamUrl, token string) (*Adapter, error) {
	netDialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: tcpKeepAlive,
	}
	// Explicit dialer so we can enforce timeouts/keepalive.
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		NetDialContext:   netDialer.DialContext,
		Proxy:            http.ProxyFromEnvironment,
	}

	// Parse the stream URL to safely append the token
	parsedUrl, err := url.Parse(streamUrl)
	if err != nil {
		return nil, fmt.Errorf("failed to parse stream url: %w", err)
	}

	// Don't append token to URL - it's sent in the OpenDataChannel message
	fullUrl := parsedUrl.String()

	debugLog("Dialing WebSocket: %s", fullUrl)
	wsConn, _, err := dialer.DialContext(ctx, fullUrl, nil)
	if err != nil {
		return nil, fmt.Errorf("websocket dial failed: %w", err)
	}

	// OpenDataChannelInput - matches session-manager-plugin format (section 5.4)
	initMsg := map[string]string{
		"MessageSchemaVersion": "1.0",
		"RequestId":            CleanUUID(uuid.New()),
		"TokenValue":           token,
		"ClientId":             CleanUUID(uuid.New()),
		"ClientVersion":        ClientVersion,
	}
	debugLog("Sending OpenDataChannel: %+v", initMsg)
	_ = wsConn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := wsConn.WriteJSON(initMsg); err != nil {
		wsConn.Close()
		return nil, fmt.Errorf("failed to send OpenDataChannel message: %w", err)
	}

	pr, pw := io.Pipe()
	adapter := &Adapter{
		conn:              wsConn,
		reader:            pr,
		writer:            pw,
		seenMsgIds:        make(map[uuid.UUID]int64),
		handshakeDone:     make(chan struct{}),
		done:              make(chan struct{}),
		incomingMsgBuffer: make(map[int64]*AgentMessage),
		expectedSeqNum:    0,
		chunkSize:         defaultStreamChunkSize,

		outgoing:                make(map[int64]*outgoingMessage),
		outgoingOldestSeq:       -1,
		rto:                     defaultRetransmissionTimeout,
		maxOutgoingUnackedBytes: defaultMaxOutgoingUnackedBytes,
	}
	adapter.outgoingCond = sync.NewCond(&adapter.outgoingMu)

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

	// Read deadline + pong handler to detect half-open connections.
	// Each pong extends the read deadline.
	_ = wsConn.SetReadDeadline(time.Now().Add(pongWait))
	wsConn.SetPongHandler(func(appData string) error {
		_ = wsConn.SetReadDeadline(time.Now().Add(pongWait))
		debugLog("WebSocket Pong received")
		return nil
	})

	go adapter.readLoop()
	go adapter.startPings()
	go adapter.resendLoop()

	return adapter, nil
}

func (a *Adapter) readLoop() {
	defer a.Close()

	for {
		msg, err := a.readMessage()
		if err != nil {
			a.writer.CloseWithError(err)
			return
		}
		if msg == nil {
			continue // unmarshal error, already logged
		}
		if !a.dispatchMessage(msg) {
			return // channel closed
		}
	}
}

// readMessage reads and parses a single message from WebSocket
func (a *Adapter) readMessage() (*AgentMessage, error) {
	_, msgBytes, err := a.conn.ReadMessage()
	if err != nil {
		log.Printf("[WARN] WS Read Error: %v", err)
		return nil, err
	}

	agentMsg, err := UnmarshalMessage(msgBytes)
	if err != nil {
		debugLog("Unmarshal Error: %v", err)
		return nil, nil // non-fatal, return nil message
	}

	// Keep per-frame logging lightweight; full per-frame logs become a bottleneck for large transfers.
	// Always log control/non-stream frames. For stream frames, sample every N messages.
	sample := true
	switch agentMsg.Header.MessageType {
	case MsgTypeOutputStreamData, MsgTypeInputStreamData:
		sample = agentMsg.Header.SequenceNumber%rxFrameLogEveryN == 0
	case MsgTypeAcknowledge:
		// ACK header SequenceNumber is always 0, so sample based on a local counter.
		a.rxAckCount++
		sample = a.rxAckCount%rxFrameLogEveryN == 0
	}
	if sample {
		debugLog("RX Frame: Type=%s Seq=%d Len=%d Flags=%d PayloadType=%d HL=%d MsgId=%s",
			agentMsg.Header.MessageType,
			agentMsg.Header.SequenceNumber,
			len(agentMsg.Payload),
			agentMsg.Header.Flags,
			agentMsg.Header.PayloadType,
			agentMsg.Header.HeaderLength,
			agentMsg.Header.MessageId.String())
	}

	// Avoid dumping binary payloads (e.g. TLS) into logs. Keep small readable payloads.
	if agentMsg.Header.MessageType == MsgTypeOutputStreamData {
		if len(agentMsg.Payload) <= maxTextPayloadLogBytes && looksMostlyText(agentMsg.Payload) {
			debugLog("Output Payload (text): %q", string(agentMsg.Payload))
		}
	}

	return agentMsg, nil
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
		a.writer.CloseWithError(io.EOF)
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

	logged := false
	for a.outgoingBytes+need > a.maxOutgoingUnackedBytes {
		if !logged {
			debugLog("Outgoing buffer full (bytes=%d need=%d limit=%d), backpressuring", a.outgoingBytes, need, a.maxOutgoingUnackedBytes)
			logged = true
		}
		a.outgoingCond.Wait()
		select {
		case <-a.done:
			return io.ErrClosedPipe
		default:
		}
	}

	select {
	case <-a.done:
		return io.ErrClosedPipe
	default:
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

		a.pauseMu.Lock()
		paused := a.paused
		a.pauseMu.Unlock()
		if paused {
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
		if time.Since(om.lastSent) <= rto {
			a.outgoingMu.Unlock()
			continue
		}

		// Mark resent before sending to avoid tight loops on very small timeouts.
		om.lastSent = time.Now()
		data = om.data
		a.outgoingMu.Unlock()

		if err := a.writeRaw(data, MsgTypeInputStreamData, PayloadTypeOutput); err != nil {
			debugLog("Resend failed: seq=%d err=%v", seq, err)
			a.Close()
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
		}
		if err := a.resendHandshakeResponse(); err != nil {
			debugLog("HandshakeResponse resend error: %v", err)
		}
		return
	}

	if err := a.handleHandshakeRequest(msg); err != nil {
		debugLog("HandshakeRequest handling error: %v", err)
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
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	// SSM uses BinaryMessage for frames
	_ = a.conn.SetWriteDeadline(time.Now().Add(writeWait))
	err := a.conn.WriteMessage(websocket.BinaryMessage, data)
	if err != nil {
		debugLog("WebSocket WriteMessage FAILED: %v", err)
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

// WaitForHandshake blocks until the SSM handshake is complete or context is cancelled
func (a *Adapter) WaitForHandshake(ctx context.Context) error {
	select {
	case <-a.handshakeDone:
		debugLog("Handshake completed, ready for data transfer")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Read implements net.Conn.Read
func (a *Adapter) Read(b []byte) (n int, err error) {
	deadlineVal := a.readDeadline.Load()
	if deadlineVal == nil {
		return a.reader.Read(b)
	}
	deadline := deadlineVal.(time.Time)
	if deadline.IsZero() {
		return a.reader.Read(b)
	}

	timeout := time.Until(deadline)
	if timeout <= 0 {
		return 0, timeoutError{}
	}

	type readResult struct {
		n   int
		err error
	}
	resultCh := make(chan readResult, 1)
	go func() {
		n, err := a.reader.Read(b)
		resultCh <- readResult{n, err}
	}()

	select {
	case res := <-resultCh:
		return res.n, res.err
	case <-time.After(timeout):
		return 0, timeoutError{}
	}
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
			time.Sleep(10 * time.Millisecond)
			continue
		}

		select {
		case <-ch:
			// resumed
			continue
		case <-a.done:
			return io.ErrClosedPipe
		}
	}
}

func (a *Adapter) Close() error {
	a.closeOnce.Do(func() {
		debugLog("Closing Adapter")
		close(a.done) // Signal that adapter is closed

		// Wake any goroutines blocked on outgoing buffer backpressure.
		if a.outgoingCond != nil {
			a.outgoingMu.Lock()
			a.outgoingCond.Broadcast()
			a.outgoingMu.Unlock()
		}

		a.reader.Close()
		a.writer.Close()
		a.conn.Close()
	})
	return nil
}

// startPings sends periodic WebSocket ping frames to keep the connection alive.
// This is required because AWS SSM service will close idle connections.
// Matches the behavior of AWS session-manager-plugin (websocketchannel.go:85-104).
func (a *Adapter) startPings() {
	debugLog("Ping loop started, interval=%v", PingInterval)
	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.done:
			debugLog("Stopping ping loop - adapter closed")
			return
		case <-ticker.C:
			deadline := time.Now().Add(writeWait)
			a.writeMu.Lock()
			err := a.conn.WriteControl(websocket.PingMessage, []byte("keepalive"), deadline)
			a.writeMu.Unlock()

			if err != nil {
				debugLog("WebSocket Ping failed: %v, closing adapter to trigger reconnect", err)
				a.Close()
				return
			}
			debugLog("WebSocket Ping sent")
		}
	}
}

// Done returns a channel that is closed when the adapter is closed.
// This can be used for lifecycle management.
func (a *Adapter) Done() <-chan struct{} {
	return a.done
}

// LocalAddr implements net.Conn
func (a *Adapter) LocalAddr() net.Addr {
	return a.conn.LocalAddr()
}

// RemoteAddr implements net.Conn
func (a *Adapter) RemoteAddr() net.Addr {
	return a.conn.RemoteAddr()
}

// SetDeadline implements net.Conn
func (a *Adapter) SetDeadline(t time.Time) error {
	a.readDeadline.Store(t)
	return a.conn.SetWriteDeadline(t)
}

// SetReadDeadline implements net.Conn
func (a *Adapter) SetReadDeadline(t time.Time) error {
	a.readDeadline.Store(t)
	return nil
}

// SetWriteDeadline implements net.Conn
func (a *Adapter) SetWriteDeadline(t time.Time) error {
	return a.conn.SetWriteDeadline(t)
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
	if _, err := a.writer.Write(msg.Payload); err != nil {
		debugLog("Pipe Write Error: %v", err)
	}
}
