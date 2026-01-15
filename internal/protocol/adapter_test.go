package protocol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{}

func TestAdapterHandshakeAndData(t *testing.T) {
	// Test timeout to prevent hanging
	testDone := make(chan struct{})
	go func() {
		select {
		case <-testDone:
			return
		case <-time.After(10 * time.Second):
			t.Error("Test timed out after 10 seconds")
		}
	}()
	defer close(testDone)

	// Track server goroutine
	var serverWg sync.WaitGroup
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()

	// 1. Setup Mock Server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverWg.Add(1)
		defer serverWg.Done()

		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("Upgrade failed: %v", err)
			return
		}
		defer ws.Close()

		// Set read deadline to prevent infinite wait
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))

		// A. Validate Init Handshake
		var initMsg map[string]string
		if err := ws.ReadJSON(&initMsg); err != nil {
			t.Logf("Failed to read init msg: %v", err)
			return
		}

		if initMsg["TokenValue"] != "test-token" {
			t.Errorf("Expected token 'test-token', got %s", initMsg["TokenValue"])
		}

		// B. Read one message and echo back
		for {
			select {
			case <-serverCtx.Done():
				return
			default:
			}

			ws.SetReadDeadline(time.Now().Add(2 * time.Second))
			mt, message, err := ws.ReadMessage()
			if err != nil {
				// Expected when client closes or timeout
				return
			}
			if mt == websocket.BinaryMessage {
				// Parse Input Frame
				agentMsg, err := UnmarshalMessage(message)
				if err != nil {
					t.Logf("Server failed unmarshal: %v", err)
					continue
				}

				if agentMsg.Header.MessageType == MsgTypeInputStreamData {
					// Echo back as Output Stream Data with Seq=0
					resp, _ := NewInputMessage(agentMsg.Payload, 0) // Seq=0 for first message
					resp.Header.MessageType = MsgTypeOutputStreamData

					data, _ := resp.MarshalBinary()
					ws.WriteMessage(websocket.BinaryMessage, data)
					// Wait a bit for client to read before exiting
					time.Sleep(100 * time.Millisecond)
					return
				}
			}
		}
	}))
	defer func() {
		serverCancel()
		server.Close()
		serverWg.Wait()
	}()

	// 2. Connect Client
	DebugMode = true
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter, err := NewAdapter(ctx, url, "test-token")
	if err != nil {
		t.Fatalf("NewAdapter failed: %v", err)
	}
	defer adapter.Close()

	// 3. Test Write (Client -> Server)
	testPayload := []byte("Hello SSM")
	if _, err := adapter.Write(testPayload); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// 4. Test Read (Server -> Client echo)
	buf := make([]byte, 1024)
	n, err := adapter.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if string(buf[:n]) != string(testPayload) {
		t.Errorf("Expected '%s', got '%s'", testPayload, buf[:n])
	}
}

func TestAdapterHandleAcknowledge_RemovesOutgoing(t *testing.T) {
	a := &Adapter{}
	a.outgoing = make(map[int64]*outgoingMessage)
	a.outgoingOldestSeq = -1
	a.maxOutgoingUnackedBytes = defaultMaxOutgoingUnackedBytes
	a.outgoingCond = sync.NewCond(&a.outgoingMu)
	a.done = make(chan struct{})

	seq := int64(42)
	msgID := uuid.New()
	a.outgoing[seq] = &outgoingMessage{msgID: msgID, data: []byte("frame"), lastSent: time.Now().Add(-300 * time.Millisecond)}
	a.outgoingOldestSeq = seq
	a.outgoingBytes = int64(len(a.outgoing[seq].data))

	ack := AcknowledgeContent{
		MessageType:         MsgTypeInputStreamData,
		MessageId:           msgID.String(),
		SequenceNumber:      seq,
		IsSequentialMessage: true,
	}
	payload, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal ack: %v", err)
	}

	a.handleAcknowledge(&AgentMessage{Header: AgentMessageHeader{MessageType: MsgTypeAcknowledge}, Payload: payload})

	if len(a.outgoing) != 0 {
		t.Fatalf("expected outgoing to be empty, got %d", len(a.outgoing))
	}
	if a.outgoingOldestSeq != -1 {
		t.Fatalf("expected outgoingOldestSeq=-1, got %d", a.outgoingOldestSeq)
	}
	if a.outgoingBytes != 0 {
		t.Fatalf("expected outgoingBytes=0, got %d", a.outgoingBytes)
	}
	if a.rto <= 0 || a.rto > maxRetransmissionTimeout {
		t.Fatalf("unexpected rto=%v", a.rto)
	}
}

func TestAdapterHandleAcknowledge_MismatchIgnored(t *testing.T) {
	a := &Adapter{}
	a.outgoing = make(map[int64]*outgoingMessage)
	a.outgoingOldestSeq = -1
	a.maxOutgoingUnackedBytes = defaultMaxOutgoingUnackedBytes
	a.outgoingCond = sync.NewCond(&a.outgoingMu)
	a.done = make(chan struct{})

	seq := int64(7)
	wantID := uuid.New()
	a.outgoing[seq] = &outgoingMessage{msgID: wantID, data: []byte("frame"), lastSent: time.Now().Add(-300 * time.Millisecond)}
	a.outgoingOldestSeq = seq
	a.outgoingBytes = int64(len(a.outgoing[seq].data))

	ack := AcknowledgeContent{
		MessageType:         MsgTypeInputStreamData,
		MessageId:           uuid.New().String(),
		SequenceNumber:      seq,
		IsSequentialMessage: true,
	}
	payload, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal ack: %v", err)
	}

	a.handleAcknowledge(&AgentMessage{Header: AgentMessageHeader{MessageType: MsgTypeAcknowledge}, Payload: payload})

	if len(a.outgoing) != 1 {
		t.Fatalf("expected outgoing size=1, got %d", len(a.outgoing))
	}
	if a.outgoingOldestSeq != seq {
		t.Fatalf("expected outgoingOldestSeq=%d, got %d", seq, a.outgoingOldestSeq)
	}
}
