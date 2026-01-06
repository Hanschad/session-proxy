package protocol

import (
	"bytes"
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

	// Message deduplication
	seenMsgIds   map[uuid.UUID]bool
	seenMsgIdsMu sync.Mutex

	// Message reordering (per AWS protocol)
	expectedSeqNum    int64                   // Next expected sequence number
	incomingMsgBuffer map[int64]*AgentMessage // Buffer for out-of-order messages
	incomingMsgBufMu  sync.Mutex              // Protects incomingMsgBuffer

	// Lifecycle management
	done      chan struct{}
	closeOnce sync.Once
}

// ClientVersion is the version reported to the SSM service
const ClientVersion = "1.2.0.0"

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
		seenMsgIds:        make(map[uuid.UUID]bool),
		handshakeDone:     make(chan struct{}),
		done:              make(chan struct{}),
		incomingMsgBuffer: make(map[int64]*AgentMessage),
		expectedSeqNum:    0,
	}

	go adapter.readLoop()

	return adapter, nil
}

func (a *Adapter) readLoop() {
	defer a.Close() // Ensure we close everything on exit

	for {
		_, msgBytes, err := a.conn.ReadMessage()
		if err != nil {
			debugLog("WS Read Error: %v", err)
			a.writer.CloseWithError(err)
			return
		}

		// Deserialize AgentMessage
		agentMsg, err := UnmarshalMessage(msgBytes)
		if err != nil {
			debugLog("Unmarshal Error: %v", err)
			continue
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

		// Check for duplicate messages
		a.seenMsgIdsMu.Lock()
		isDuplicate := a.seenMsgIds[agentMsg.Header.MessageId]
		if !isDuplicate {
			a.seenMsgIds[agentMsg.Header.MessageId] = true
		}
		a.seenMsgIdsMu.Unlock()

		switch agentMsg.Header.MessageType {
		case MsgTypeOutputStreamData:
			// Handle based on PayloadType
			switch agentMsg.Header.PayloadType {
			case PayloadTypeHandshakeRequest:
				// SSM Agent handshake - respond and ACK
				debugLog("Received HandshakeRequest: %s", string(agentMsg.Payload))
				// Per official plugin: always ACK + Response (even for retransmissions)
				if isDuplicate || a.handshakeResponded {
					debugLog("Received duplicate HandshakeRequest, resending ACK + Response")
					if err := a.sendAck(agentMsg); err != nil {
						debugLog("Ack Send Error: %v", err)
					}
					// Also resend HandshakeResponse (in case first one was lost)
					if err := a.resendHandshakeResponse(); err != nil {
						debugLog("HandshakeResponse resend error: %v", err)
					}
				} else if err := a.handleHandshakeRequest(agentMsg); err != nil {
					debugLog("HandshakeRequest handling error: %v", err)
				} else {
					// Successfully processed HandshakeRequest - update expected sequence
					a.incomingMsgBufMu.Lock()
					a.expectedSeqNum = agentMsg.Header.SequenceNumber + 1
					a.incomingMsgBufMu.Unlock()
					debugLog("Updated expectedSeqNum to %d after HandshakeRequest", a.expectedSeqNum)
				}

			case PayloadTypeHandshakeComplete:
				// Handshake complete - session ready
				debugLog("Received HandshakeComplete: %s", string(agentMsg.Payload))
				if !a.handshakeComplete {
					a.handshakeComplete = true
					close(a.handshakeDone) // Signal that handshake is complete
					// Update expected sequence number
					a.incomingMsgBufMu.Lock()
					a.expectedSeqNum = agentMsg.Header.SequenceNumber + 1
					a.incomingMsgBufMu.Unlock()
					debugLog("Updated expectedSeqNum to %d after HandshakeComplete", a.expectedSeqNum)
				}
				if err := a.sendAck(agentMsg); err != nil {
					debugLog("Ack Send Error: %v", err)
				}

			default:
				// Check for SSM Handshake JSON (fallback for older agent versions)
				if bytes.Contains(agentMsg.Payload, []byte("AgentVersion")) ||
					bytes.Contains(agentMsg.Payload, []byte("RequestedClientActions")) {
					debugLog("Ignored Agent Handshake Message (legacy): %s", string(agentMsg.Payload))
					if err := a.sendAck(agentMsg); err != nil {
						debugLog("Ack Send Error: %v", err)
					}
				} else {
					// Normal data - process with sequence ordering
					a.handleDataMessage(agentMsg)
				}
			}

		case MsgTypeAcknowledge:
			// Received ACK for our sent message - could update retransmission state
			debugLog("Received ACK: %s", string(agentMsg.Payload))

		case MsgTypeChannelClosed:
			debugLog("Channel closed by remote")
			a.writer.CloseWithError(io.EOF)
			return

		default:
			debugLog("Ignored Message Type: %s", agentMsg.Header.MessageType)
		}
	}
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
