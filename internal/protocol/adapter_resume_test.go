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

func TestAdapterResume_RetransmitsUnackedAfterDrop(t *testing.T) {
	var (
		mu                sync.Mutex
		resumeCalls       atomic.Int32
		connN             atomic.Int32
		postResumePayload []byte
		postResumeReady   = make(chan struct{})
		postResumeOnce    sync.Once
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()

		id := connN.Add(1)
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
			if err != nil || agentMsg.Header.MessageType != MsgTypeInputStreamData {
				continue
			}

			if id == 1 {
				// First connection: accept the frame but never ACK so it stays
				// in the adapter outgoing buffer for post-resume retransmission.
				continue
			}

			mu.Lock()
			postResumePayload = append([]byte(nil), agentMsg.Payload...)
			mu.Unlock()
			postResumeOnce.Do(func() { close(postResumeReady) })

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

	// Give the first connection time to receive (but not ACK) the frame.
	time.Sleep(50 * time.Millisecond)
	adapter.ForceCloseTransport()

	select {
	case <-postResumeReady:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for post-resume retransmission")
	}

	if resumeCalls.Load() == 0 {
		t.Fatal("expected ResumeFunc to be called after transport drop")
	}
	mu.Lock()
	got := string(postResumePayload)
	mu.Unlock()
	if got != string(payloadBytes) {
		t.Fatalf("expected post-resume payload %q, got %q", payloadBytes, got)
	}
	if adapter.Reconnecting() {
		t.Fatal("adapter still reconnecting after successful resume")
	}
}

func TestAdapterResume_WriteSucceedsAcrossTransportDrop(t *testing.T) {
	var (
		mu              sync.Mutex
		resumeCalls     atomic.Int32
		connN           atomic.Int32
		receivedPayload []byte
		receivedReady   = make(chan struct{})
		receivedOnce    sync.Once
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()

		id := connN.Add(1)
		var initMsg map[string]string
		if err := ws.ReadJSON(&initMsg); err != nil {
			return
		}

		// First connection exists only to be force-closed; second accepts data.
		if id == 1 {
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
			}
		}

		for {
			_, message, err := ws.ReadMessage()
			if err != nil {
				return
			}
			agentMsg, err := UnmarshalMessage(message)
			if err != nil || agentMsg.Header.MessageType != MsgTypeInputStreamData {
				continue
			}
			mu.Lock()
			receivedPayload = append([]byte(nil), agentMsg.Payload...)
			mu.Unlock()
			receivedOnce.Do(func() { close(receivedReady) })

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

	adapter.ForceCloseTransport()

	writeErr := make(chan error, 1)
	go func() {
		_, err := adapter.Write([]byte("after-drop"))
		writeErr <- err
	}()

	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatalf("Write across resume failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked across resume")
	}

	select {
	case <-receivedReady:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for write after resume")
	}

	if resumeCalls.Load() == 0 {
		t.Fatal("expected ResumeFunc to be called")
	}
	mu.Lock()
	got := string(receivedPayload)
	mu.Unlock()
	if got != "after-drop" {
		t.Fatalf("expected payload %q, got %q", "after-drop", got)
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
