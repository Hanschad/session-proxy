package protocol

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
		fmt.Printf("[DEBUG] "+format+"\n", args...)
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
	handshakeComplete  bool
	handshakeResponded bool // Track if we already responded to HandshakeRequest
	handshakeDone      chan struct{}

	// Message deduplication
	seenMsgIds   map[uuid.UUID]bool
	seenMsgIdsMu sync.Mutex

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

	clientId := uuid.New().String()

	// OpenDataChannelInput - matches session-manager-plugin format (section 5.4)
	initMsg := map[string]string{
		"MessageSchemaVersion": "1.0",
		"RequestId":            uuid.New().String(),
		"TokenValue":           token,
		"ClientId":             clientId,
		"ClientVersion":        ClientVersion,
	}
	debugLog("Sending OpenDataChannel: %+v", initMsg)
	if err := wsConn.WriteJSON(initMsg); err != nil {
		wsConn.Close()
		return nil, fmt.Errorf("failed to send OpenDataChannel message: %w", err)
	}

	pr, pw := io.Pipe()
	adapter := &Adapter{
		conn:          wsConn,
		reader:        pr,
		writer:        pw,
		seenMsgIds:    make(map[uuid.UUID]bool),
		handshakeDone: make(chan struct{}),
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
				// Only respond once, but always send ACK for retransmissions
				if isDuplicate || a.handshakeResponded {
					debugLog("Skipping duplicate HandshakeRequest, sending ACK only")
					if err := a.sendAck(agentMsg); err != nil {
						debugLog("Ack Send Error: %v", err)
					}
				} else if err := a.handleHandshakeRequest(agentMsg); err != nil {
					debugLog("HandshakeRequest handling error: %v", err)
				}

			case PayloadTypeHandshakeComplete:
				// Handshake complete - session ready
				debugLog("Received HandshakeComplete: %s", string(agentMsg.Payload))
				if !a.handshakeComplete {
					a.handshakeComplete = true
					close(a.handshakeDone) // Signal that handshake is complete
				}
				if err := a.sendAck(agentMsg); err != nil {
					debugLog("Ack Send Error: %v", err)
				}

			default:
				// Check for SSM Handshake JSON (fallback for older agent versions)
				if bytes.Contains(agentMsg.Payload, []byte("AgentVersion")) ||
					bytes.Contains(agentMsg.Payload, []byte("RequestedClientActions")) {
					debugLog("Ignored Agent Handshake Message (legacy): %s", string(agentMsg.Payload))
				} else {
					// Normal data - forward to reader
					if _, err := a.writer.Write(agentMsg.Payload); err != nil {
						debugLog("Pipe Write Error: %v", err)
						return
					}
				}

				// Send Ack for all output_stream_data messages
				if err := a.sendAck(agentMsg); err != nil {
					debugLog("Ack Send Error: %v", err)
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
	responseJson := fmt.Sprintf(`{"ClientVersion":"%s","ProcessedClientActions":[{"ActionType":"SessionType","ActionStatus":1}],"Errors":[]}`, ClientVersion)
	payloadBytes := []byte(responseJson)

	responseMsg := &AgentMessage{
		Header: AgentMessageHeader{
			HeaderLength:   uint32(HeaderLengthValue),
			MessageType:    MsgTypeInputStreamData,
			SchemaVersion:  SchemaVersion,
			CreatedDate:    uint64(time.Now().UnixMilli()),
			SequenceNumber: a.nextSeq(),
			Flags:          FlagData, // Per plugin: flag = 0 for SendInputDataMessage
			MessageId:      uuid.New(),
			PayloadType:    PayloadTypeHandshakeResponse,
			PayloadLength:  uint32(len(payloadBytes)),
		},
		Payload: payloadBytes,
	}

	debugLog("TX HandshakeResponse: %s", responseJson)
	
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
	
	return a.writeMessage(responseMsg)
}

func (a *Adapter) sendAck(orig *AgentMessage) error {
	ack, err := NewAcknowledgeMessage(orig.Header.MessageType, orig.Header.MessageId, orig.Header.SequenceNumber)
	if err != nil {
		return err
	}
	
	// Debug: show the actual bytes we're sending
	ackData, _ := ack.MarshalBinary()
	debugLog("TX ACK for MsgId=%s Seq=%d", orig.Header.MessageId.String(), orig.Header.SequenceNumber)
	debugLog("TX ACK Header: HL=%d Type=%s Ver=%d Seq=%d Flags=%d PayloadType=%d PayloadLen=%d",
		ack.Header.HeaderLength, ack.Header.MessageType, ack.Header.SchemaVersion,
		ack.Header.SequenceNumber, ack.Header.Flags, ack.Header.PayloadType, ack.Header.PayloadLength)
	debugLog("TX ACK Payload: %q", string(ack.Payload))
	debugLog("TX ACK Total bytes: %d, first 20: %x", len(ackData), ackData[:min(20, len(ackData))])
	
	return a.writeMessage(ack)
}

func (a *Adapter) writeMessage(msg *AgentMessage) error {
	data, err := msg.MarshalBinary()
	if err != nil {
		return err
	}

	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	// SSM uses BinaryMessage for frames
	return a.conn.WriteMessage(websocket.BinaryMessage, data)
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
		a.reader.Close()
		a.writer.Close()
		a.conn.Close()
	})
	return nil
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
