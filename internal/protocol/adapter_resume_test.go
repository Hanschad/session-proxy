package protocol

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func TestAdapterResume_ReconnectsAfterTransportDrop(t *testing.T) {
	var (
		mu              sync.Mutex
		resumeCalls     atomic.Int32
		activeConn      *websocket.Conn
		receivedPayload []byte
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		mu.Lock()
		if activeConn != nil {
			_ = activeConn.Close()
		}
		activeConn = ws
		mu.Unlock()

		defer ws.Close()

		var initMsg map[string]string
		if err := ws.ReadJSON(&initMsg); err != nil {
			return
		}

		for {
			_, message, err := ws.ReadMessage()
			if err != nil {
				return
			}
			agentMsg, err := UnmarshalMessage(message)
			if err != nil {
				continue
			}
			if agentMsg.Header.MessageType != MsgTypeInputStreamData {
				continue
			}
			mu.Lock()
			receivedPayload = append([]byte(nil), agentMsg.Payload...)
			mu.Unlock()
			ack, err := NewAcknowledgeMessage(agentMsg.Header.MessageType, agentMsg.Header.MessageId, agentMsg.Header.SequenceNumber)
			if err != nil {
				return
			}
			ackData, err := ack.MarshalBinary()
			if err != nil {
				return
			}
			_ = ws.WriteMessage(websocket.BinaryMessage, ackData)
			return
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	adapter, err := NewAdapter(ctx, wsURL, "token-1", WithResume(func(context.Context) (string, string, error) {
		resumeCalls.Add(1)
		return wsURL, "token-resumed", nil
	}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	defer adapter.Close()

	payloadBytes := []byte("resume-me")
	if _, err := adapter.Write(payloadBytes); err != nil {
		t.Fatalf("Write: %v", err)
	}

	adapter.ForceCloseTransport()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		payload := string(receivedPayload)
		mu.Unlock()
		if resumeCalls.Load() > 0 && payload == string(payloadBytes) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	payload := string(receivedPayload)
	mu.Unlock()
	if resumeCalls.Load() == 0 {
		t.Fatal("expected ResumeFunc to be called after transport drop")
	}
	if payload != string(payloadBytes) {
		t.Fatalf("expected payload %q after resume, got %q", payloadBytes, payload)
	}
	if adapter.Reconnecting() {
		t.Fatal("adapter still reconnecting after successful resume")
	}
}

func TestAdapterResume_DisabledClosesOnTransportDrop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		var initMsg map[string]string
		_ = ws.ReadJSON(&initMsg)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Setenv("SESSION_PROXY_SSM_RESUME", "off")

	adapter, err := NewAdapter(ctx, wsURL, "token-1", WithResume(func(context.Context) (string, string, error) {
		t.Fatal("resume should not be called when disabled")
		return "", "", nil
	}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}

	adapter.ForceCloseTransport()

	select {
	case <-adapter.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("expected adapter to close when resume is disabled")
	}
}

func TestAdapterResume_BudgetExhaustedClosesAdapter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		var initMsg map[string]string
		_ = ws.ReadJSON(&initMsg)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Setenv("SESSION_PROXY_SSM_RESUME_BUDGET", "200ms")

	adapter, err := NewAdapter(ctx, wsURL, "token-1", WithResume(func(context.Context) (string, string, error) {
		return "", "", errors.New("resume unavailable")
	}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}

	adapter.ForceCloseTransport()

	select {
	case <-adapter.Done():
		if !errors.Is(adapter.CloseReason(), errResumeBudgetExhausted) {
			t.Fatalf("expected budget exhausted close reason, got %v", adapter.CloseReason())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expected adapter to close after resume budget exhausted")
	}
}

func TestAdapterResume_ChannelClosedDoesNotResume(t *testing.T) {
	var resumeCalls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		var initMsg map[string]string
		_ = ws.ReadJSON(&initMsg)

		closed := &AgentMessage{
			Header: AgentMessageHeader{
				HeaderLength:   uint32(HeaderLengthValue),
				MessageType:    MsgTypeChannelClosed,
				SchemaVersion:  SchemaVersion,
				CreatedDate:    uint64(time.Now().UnixMilli()),
				SequenceNumber: 0,
				MessageId:      uuid.New(),
				PayloadLength:  0,
			},
		}
		data, err := closed.MarshalBinary()
		if err != nil {
			return
		}
		_ = ws.WriteMessage(websocket.BinaryMessage, data)
		time.Sleep(500 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter, err := NewAdapter(ctx, wsURL, "token-1", WithResume(func(context.Context) (string, string, error) {
		resumeCalls.Add(1)
		return wsURL, "token-resumed", nil
	}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}

	select {
	case <-adapter.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("expected adapter to close on channel_closed")
	}

	if resumeCalls.Load() > 0 {
		t.Fatal("channel_closed must not trigger resume")
	}
	if !errors.Is(adapter.CloseReason(), errChannelClosedByRemote) {
		t.Fatalf("expected channel_closed reason, got %v", adapter.CloseReason())
	}
}
