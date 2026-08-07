package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
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

func TestWaitForHandshake_AdapterClosedBeforeComplete(t *testing.T) {
	a := &Adapter{
		handshakeDone: make(chan struct{}),
		done:          make(chan struct{}),
	}

	// Close adapter before handshake completes
	close(a.done)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := a.WaitForHandshake(ctx)
	if err == nil {
		t.Fatal("expected error when adapter closed before handshake")
	}
	if err == context.DeadlineExceeded {
		t.Fatal("should not wait for context timeout; should fail fast on adapter close")
	}
}

func TestWaitForHandshake_ReturnsCloseReason(t *testing.T) {
	want := io.ErrUnexpectedEOF
	a := &Adapter{
		handshakeDone: make(chan struct{}),
		done:          make(chan struct{}),
	}
	a.closeWithError(want)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := a.WaitForHandshake(ctx)
	if !errors.Is(err, errAdapterClosedBeforeHandshake) {
		t.Fatalf("expected handshake-close wrapper, got %v", err)
	}
	if !errors.Is(err, want) {
		t.Fatalf("expected close reason %v, got %v", want, err)
	}
}

func TestWaitForHandshake_PrefersCompletedHandshake(t *testing.T) {
	a := &Adapter{
		handshakeDone: make(chan struct{}),
		done:          make(chan struct{}),
	}
	close(a.handshakeDone)
	close(a.done)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := a.WaitForHandshake(ctx); err != nil {
		t.Fatalf("expected handshake success to win, got %v", err)
	}
}

func TestReadDeadlineUsesStreamDeadlineWithoutGoroutineBridge(t *testing.T) {
	readConn, writeConn := net.Pipe()
	a := &Adapter{
		streamReader: readConn,
		streamWriter: writeConn,
		done:         make(chan struct{}),
	}
	defer a.Close()

	if err := a.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}

	buf := make([]byte, 8)
	start := time.Now()
	_, err := a.Read(buf)
	if err == nil {
		t.Fatal("expected timeout error")
	}

	var netErr interface{ Timeout() bool }
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("expected timeout error, got %v", err)
	}

	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("expected read deadline to fire promptly, got %s", elapsed)
	}
}

func TestHandleHandshakeRequestPayload_WriteFailureClosesAdapter(t *testing.T) {
	a, cleanup := newBareTestAdapter(t)
	defer cleanup()

	if err := a.conn.Close(); err != nil {
		t.Fatalf("close websocket: %v", err)
	}

	msg := &AgentMessage{
		Header: AgentMessageHeader{
			MessageType:    MsgTypeOutputStreamData,
			SequenceNumber: 1,
			MessageId:      uuid.New(),
			PayloadType:    PayloadTypeHandshakeRequest,
		},
		Payload: []byte(`{"MessageSchemaVersion":"1.0"}`),
	}

	a.handleHandshakeRequestPayload(msg, false)

	select {
	case <-a.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("adapter did not close after handshake write failure")
	}

	if err := a.CloseReason(); err == nil {
		t.Fatal("expected close reason after handshake write failure")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := a.WaitForHandshake(ctx)
	if !errors.Is(err, errAdapterClosedBeforeHandshake) {
		t.Fatalf("expected handshake-close wrapper, got %v", err)
	}
}

func TestAddOutgoing_ClosedAdapter(t *testing.T) {
	a, cleanup := newTestAdapter(t)
	defer cleanup()

	// Fill the buffer to trigger backpressure
	a.outgoing[0] = &outgoingMessage{msgID: uuid.New(), data: make([]byte, 100), lastSent: time.Now()}
	a.outgoingBytes = 100
	a.outgoingOldestSeq = 0
	a.maxOutgoingUnackedBytes = 100

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.addOutgoing(1, uuid.New(), make([]byte, 10))
	}()

	// Give the goroutine time to enter Wait()
	time.Sleep(50 * time.Millisecond)

	// Close the adapter through the public path — should wake the blocked writer.
	a.Close()

	select {
	case err := <-errCh:
		if err != io.ErrClosedPipe {
			t.Fatalf("expected io.ErrClosedPipe, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("addOutgoing did not return after adapter close; deadlock")
	}
}

func TestWrite_WriteDeadlineDuringPause(t *testing.T) {
	a := &Adapter{
		done:     make(chan struct{}),
		pauseCh:  make(chan struct{}),
		paused:   true,
		outgoing: make(map[int64]*outgoingMessage),
	}

	if err := a.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetWriteDeadline() error = %v", err)
	}

	start := time.Now()
	_, err := a.Write([]byte("hello"))
	if err == nil {
		t.Fatal("expected timeout error")
	}

	var netErr interface{ Timeout() bool }
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("expected timeout error, got %v", err)
	}

	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("expected write deadline to fire promptly, got %s", elapsed)
	}
}

func TestAddOutgoing_WriteDeadlineDuringBackpressure(t *testing.T) {
	a := &Adapter{
		done:                    make(chan struct{}),
		outgoing:                make(map[int64]*outgoingMessage),
		outgoingOldestSeq:       0,
		maxOutgoingUnackedBytes: 100,
	}
	a.outgoingCond = sync.NewCond(&a.outgoingMu)
	a.outgoing[0] = &outgoingMessage{msgID: uuid.New(), data: make([]byte, 100), lastSent: time.Now()}
	a.outgoingBytes = 100

	if err := a.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetWriteDeadline() error = %v", err)
	}

	start := time.Now()
	err := a.addOutgoing(1, uuid.New(), make([]byte, 10))
	if err == nil {
		t.Fatal("expected timeout error")
	}

	var netErr interface{ Timeout() bool }
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("expected timeout error, got %v", err)
	}

	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("expected addOutgoing deadline to fire promptly, got %s", elapsed)
	}
}

func newBareTestAdapter(t *testing.T) (*Adapter, func()) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		server.Close()
		t.Fatalf("Dial websocket: %v", err)
	}

	reader, writer := net.Pipe()
	adapter := &Adapter{
		conn:                    conn,
		streamReader:            reader,
		streamWriter:            writer,
		chunkSize:               defaultStreamChunkSize,
		seenMsgIds:              make(map[uuid.UUID]int64),
		handshakeDone:           make(chan struct{}),
		done:                    make(chan struct{}),
		incomingMsgBuffer:       make(map[int64]*AgentMessage),
		outgoing:                make(map[int64]*outgoingMessage),
		outgoingOldestSeq:       -1,
		rto:                     defaultRetransmissionTimeout,
		maxOutgoingUnackedBytes: defaultMaxOutgoingUnackedBytes,
	}
	adapter.outgoingCond = sync.NewCond(&adapter.outgoingMu)

	cleanup := func() {
		adapter.Close()
		server.Close()
	}
	return adapter, cleanup
}

// newTestAdapter creates a minimal Adapter backed by a real WebSocket for
// behavior-level tests. The returned server and cleanup func must be deferred.
func newTestAdapter(t *testing.T) (*Adapter, func()) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		// Read and discard — acts as a black hole (never ACKs).
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter, err := NewAdapter(ctx, url, "test-token")
	if err != nil {
		server.Close()
		t.Fatalf("NewAdapter: %v", err)
	}

	cleanup := func() {
		adapter.Close()
		server.Close()
	}
	return adapter, cleanup
}

func TestResendLoop_MaxAttemptsClosesAdapter(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()

	// Build a valid binary frame so writeRaw succeeds.
	msg, err := NewInputMessage([]byte("test"), 0)
	if err != nil {
		t.Fatalf("NewInputMessage: %v", err)
	}
	frame, err := msg.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}

	// Inject a message that is already near the resend limit with an expired RTO
	// so the resendLoop will hit the cutoff within a few ticks.
	adapter.outgoingMu.Lock()
	adapter.outgoing[0] = &outgoingMessage{
		msgID:       msg.Header.MessageId,
		data:        frame,
		lastSent:    time.Now().Add(-10 * time.Second),
		resendCount: maxResendAttempts, // Already at limit
	}
	adapter.outgoingOldestSeq = 0
	adapter.outgoingBytes = int64(len(frame))
	adapter.outgoingMu.Unlock()

	// resendLoop is already running (started by NewAdapter).
	// Wait for the adapter to close itself.
	select {
	case <-adapter.Done():
		// Success — resendLoop detected the exhausted budget and closed the adapter.
	case <-time.After(5 * time.Second):
		t.Fatal("adapter was not closed after exceeding maxResendAttempts; resendLoop behavior regression")
	}
}

func TestPausePublication_BoundedTimeout(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()

	// Pause publication and backdate pausedSince so the resendLoop sees it as expired.
	adapter.pausePublication()

	adapter.pauseMu.Lock()
	if !adapter.paused {
		adapter.pauseMu.Unlock()
		t.Fatal("expected paused=true after pausePublication()")
	}
	adapter.pausedSince = time.Now().Add(-maxPauseTime - 1*time.Second)
	adapter.pauseMu.Unlock()

	// resendLoop is already running; it should detect the expired pause and close.
	select {
	case <-adapter.Done():
		// Success — adapter was closed due to prolonged pause.
	case <-time.After(5 * time.Second):
		t.Fatal("adapter was not closed after exceeding maxPauseTime; pause timeout behavior regression")
	}
}

// TestProcessMessage_DropsNonOutputPayloads verifies that control payloads
// (Flag, Error, ExitCode, ...) are never injected into the tunneled byte
// stream. Injecting them corrupts the stream (e.g. SSH "banner exchange ...
// invalid format" when garbage precedes the SSH identification string).
func TestProcessMessage_DropsNonOutputPayloads(t *testing.T) {
	pr, pw := net.Pipe()
	defer pr.Close()
	defer pw.Close()

	a := &Adapter{streamWriter: pw}

	banner := []byte("SSH-2.0-OpenSSH_9.7\r\n")
	go func() {
		a.processMessage(&AgentMessage{
			Header:  AgentMessageHeader{MessageType: MsgTypeOutputStreamData, PayloadType: PayloadTypeFlag, SequenceNumber: 0},
			Payload: []byte{0, 0, 0, 1},
		})
		a.processMessage(&AgentMessage{
			Header:  AgentMessageHeader{MessageType: MsgTypeOutputStreamData, PayloadType: PayloadTypeError, SequenceNumber: 1},
			Payload: []byte("Connection to destination port failed"),
		})
		a.processMessage(&AgentMessage{
			Header:  AgentMessageHeader{MessageType: MsgTypeOutputStreamData, PayloadType: PayloadTypeOutput, SequenceNumber: 2},
			Payload: banner,
		})
	}()

	if err := pr.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 128)
	n, err := pr.Read(buf)
	if err != nil {
		t.Fatalf("read from stream: %v", err)
	}
	if got := string(buf[:n]); got != string(banner) {
		t.Fatalf("expected only output payload %q on stream, got %q", banner, got)
	}
}
