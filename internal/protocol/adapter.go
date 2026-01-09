package protocol

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"sync"
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

// Adapter implements net.Conn over an SSM WebSocket session
type Adapter struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	seqNum  int64

	// Read-side pipe (replaces complex buffering)
	reader *io.PipeReader
	writer *io.PipeWriter

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

func NewAdapter(ctx context.Context, streamUrl, token string) (*Adapter, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
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
	}

	// Set PongHandler to verify server is responding to our pings
	wsConn.SetPongHandler(func(appData string) error {
		debugLog("WebSocket Pong received")
		return nil
	})

	go adapter.readLoop()
	go adapter.startPings()

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

	debugLog("RX Frame: Type=%s Seq=%d Len=%d Flags=%d PayloadType=%d HL=%d MsgId=%s",
		agentMsg.Header.MessageType,
		agentMsg.Header.SequenceNumber,
		len(agentMsg.Payload),
		agentMsg.Header.Flags,
		agentMsg.Header.PayloadType,
		agentMsg.Header.HeaderLength,
		agentMsg.Header.MessageId.String())
	if agentMsg.Header.MessageType == MsgTypeOutputStreamData || agentMsg.Header.MessageType == MsgTypeAcknowledge {
		debugLog("Payload: %q", string(agentMsg.Payload))
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
		debugLog("Received ACK: %s", string(msg.Payload))
	case MsgTypeChannelClosed:
		debugLog("Channel closed by remote")
		a.writer.CloseWithError(io.EOF)
		return false
	default:
		debugLog("Ignored Message Type: %s", msg.Header.MessageType)
	}
	return true
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

	// Debug: show the actual bytes we're sending
	ackData, _ := ack.MarshalBinary()
	debugLog("TX ACK for MsgId=%s Seq=%d", orig.Header.MessageId.String(), orig.Header.SequenceNumber)
	debugLog("TX ACK Header: HL=%d Type=%q Ver=%d Seq=%d Flags=%d PayloadType=%d PayloadLen=%d",
		ack.Header.HeaderLength, ack.Header.MessageType, ack.Header.SchemaVersion,
		ack.Header.SequenceNumber, ack.Header.Flags, ack.Header.PayloadType, ack.Header.PayloadLength)
	debugLog("TX ACK Payload JSON: %s", string(ack.Payload))
	debugLog("TX ACK Total bytes: %d, first 40: %x", len(ackData), ackData[:min(40, len(ackData))])

	return a.writeMessage(ack)
}

func (a *Adapter) writeMessage(msg *AgentMessage) error {
	data, err := msg.MarshalBinary()
	if err != nil {
		debugLog("writeMessage MarshalBinary error: %v", err)
		return err
	}

	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	// SSM uses BinaryMessage for frames
	err = a.conn.WriteMessage(websocket.BinaryMessage, data)
	if err != nil {
		debugLog("WebSocket WriteMessage FAILED: %v", err)
	} else {
		debugLog("WebSocket WriteMessage OK: %d bytes, MsgType=%q", len(data), msg.Header.MessageType)
	}
	return err
}

func (a *Adapter) nextSeq() int64 {
	seq := a.seqNum
	a.seqNum++
	return seq
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
	// Simple delegation to pipe reader
	return a.reader.Read(b)
}

// Write implements net.Conn.Write
func (a *Adapter) Write(b []byte) (n int, err error) {
	debugLog("TX SSH Data: %d bytes", len(b))

	chunkSize := 1024
	totalWritten := 0

	for len(b) > 0 {
		sendLen := len(b)
		if sendLen > chunkSize {
			sendLen = chunkSize
		}

		chunk := b[:sendLen]
		msg, err := NewInputMessage(chunk, a.nextSeq())
		if err != nil {
			return totalWritten, err
		}

		if err := a.writeMessage(msg); err != nil {
			return totalWritten, err
		}

		totalWritten += sendLen
		b = b[sendLen:]
	}

	return totalWritten, nil
}

func (a *Adapter) Close() error {
	a.closeOnce.Do(func() {
		debugLog("Closing Adapter")
		close(a.done) // Signal that adapter is closed
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
			a.writeMu.Lock()
			err := a.conn.WriteMessage(websocket.PingMessage, []byte("keepalive"))
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
	return a.conn.SetReadDeadline(t)
}

// SetReadDeadline implements net.Conn
func (a *Adapter) SetReadDeadline(t time.Time) error {
	return a.conn.SetReadDeadline(t)
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
