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

	closeOnce sync.Once
}

func NewAdapter(ctx context.Context, streamUrl, token string) (*Adapter, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	// Parse the stream URL to safely append the token
	parsedUrl, err := url.Parse(streamUrl)
	if err != nil {
		return nil, fmt.Errorf("failed to parse stream url: %w", err)
	}

	if token != "" {
		q := parsedUrl.Query()
		q.Set("token", token)
		parsedUrl.RawQuery = q.Encode()
	}

	fullUrl := parsedUrl.String()

	debugLog("Dialing WebSocket: %s", fullUrl)
	wsConn, _, err := dialer.DialContext(ctx, fullUrl, nil)
	if err != nil {
		return nil, fmt.Errorf("websocket dial failed: %w", err)
	}

	// INIT HANDSHAKE
	// Based on session-manager-plugin, the keys are:
	// MessageSchemaVersion, RequestId, TokenValue
	initMsg := map[string]string{
		"MessageSchemaVersion": "1.0",
		"RequestId":            uuid.New().String(),
		"TokenValue":           token,
	}
	debugLog("Sending Init Handshake: %+v", initMsg)
	if err := wsConn.WriteJSON(initMsg); err != nil {
		wsConn.Close()
		return nil, fmt.Errorf("failed to send init message: %w", err)
	}

	pr, pw := io.Pipe()
	adapter := &Adapter{
		conn:   wsConn,
		reader: pr,
		writer: pw,
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

		debugLog("RX Frame: Type=%s Seq=%d Len=%d Flags=%d PayloadType=%d",
			agentMsg.Header.MessageType,
			agentMsg.Header.SequenceNumber,
			len(agentMsg.Payload),
			agentMsg.Header.Flags,
			agentMsg.Header.PayloadType)
		if agentMsg.Header.MessageType == MsgTypeOutputStreamData || agentMsg.Header.MessageType == MsgTypeAcknowledge {
			debugLog("Payload: %q", string(agentMsg.Payload))
		}

		switch agentMsg.Header.MessageType {
		case MsgTypeOutputStreamData:
			// Check for SSM Handshake (RequestedClientActions)
			// This usually comes as the first message and contains JSON like {"AgentVersion":"..."}
			// We must filter this out so the SSH client doesn't see it.
			if bytes.Contains(agentMsg.Payload, []byte("AgentVersion")) {
				debugLog("Ignored Agent Handshake Message: %s", string(agentMsg.Payload))
			} else {
				// Write payload to pipe. This will block if reader isn't reading, providing backpressure.
				if _, err := a.writer.Write(agentMsg.Payload); err != nil {
					debugLog("Pipe Write Error: %v", err)
					return
				}
			}

			// Send Ack
			if err := a.sendAck(agentMsg); err != nil {
				debugLog("Ack Send Error: %v", err)
			}

		case MsgTypeAcknowledge:
			// No-op for now
		default:
			debugLog("Ignored Message Type: %s", agentMsg.Header.MessageType)
		}
	}
}

func (a *Adapter) sendAck(orig *AgentMessage) error {
	ack, err := NewAcknowledgeMessage(orig.Header.MessageId, orig.Header.SequenceNumber, a.nextSeq())
	if err != nil {
		return err
	}
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
	a.seqNum++
	return a.seqNum
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
