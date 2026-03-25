package socks5

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestAddrParseIPv4(t *testing.T) {
	// IPv4: 192.168.1.1:8080
	data := []byte{
		AtypIPv4,
		192, 168, 1, 1,
		0x1F, 0x90, // 8080 big-endian
	}

	addr, err := ReadAddr(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadAddr failed: %v", err)
	}

	if addr.Type != AtypIPv4 {
		t.Errorf("expected AtypIPv4, got %d", addr.Type)
	}
	if !addr.IP.Equal(net.IPv4(192, 168, 1, 1)) {
		t.Errorf("expected 192.168.1.1, got %s", addr.IP)
	}
	if addr.Port != 8080 {
		t.Errorf("expected port 8080, got %d", addr.Port)
	}
	if addr.String() != "192.168.1.1:8080" {
		t.Errorf("expected 192.168.1.1:8080, got %s", addr.String())
	}
}

func TestAddrParseDomain(t *testing.T) {
	// Domain: example.com:443
	domain := "example.com"
	data := []byte{
		AtypDomain,
		byte(len(domain)),
	}
	data = append(data, domain...)
	data = append(data, 0x01, 0xBB) // 443 big-endian

	addr, err := ReadAddr(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadAddr failed: %v", err)
	}

	if addr.Type != AtypDomain {
		t.Errorf("expected AtypDomain, got %d", addr.Type)
	}
	if addr.Domain != "example.com" {
		t.Errorf("expected example.com, got %s", addr.Domain)
	}
	if addr.Port != 443 {
		t.Errorf("expected port 443, got %d", addr.Port)
	}
}

func TestAddrWriteTo(t *testing.T) {
	addr := &Addr{
		Type: AtypIPv4,
		IP:   net.IPv4(127, 0, 0, 1),
		Port: 1080,
	}

	var buf bytes.Buffer
	_, err := addr.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo failed: %v", err)
	}

	expected := []byte{AtypIPv4, 127, 0, 0, 1, 0x04, 0x38}
	if !bytes.Equal(buf.Bytes(), expected) {
		t.Errorf("expected %v, got %v", expected, buf.Bytes())
	}
}

func TestHandshakeNoAuth(t *testing.T) {
	srv := New(&Config{})

	// Client sends: VER(5) + NMETHODS(1) + METHODS(NoAuth)
	clientData := []byte{Version, 0x01, MethodNoAuth}

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan error)
	go func() {
		done <- srv.handshake(server)
	}()

	// Send client data
	client.Write(clientData)

	// Read server response
	resp := make([]byte, 2)
	if _, err := io.ReadFull(client, resp); err != nil {
		t.Fatalf("read response: %v", err)
	}

	if resp[0] != Version || resp[1] != MethodNoAuth {
		t.Errorf("expected [5, 0], got %v", resp)
	}

	if err := <-done; err != nil {
		t.Errorf("handshake error: %v", err)
	}
}

func TestConnect(t *testing.T) {
	// Start a mock target server
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer targetLn.Close()

	go func() {
		conn, _ := targetLn.Accept()
		defer conn.Close()
		io.Copy(conn, conn) // Echo server
	}()

	// Start SOCKS5 server
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer socksLn.Close()

	srv := New(&Config{
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, addr)
		},
	})
	go srv.Serve(socksLn)

	// Connect to SOCKS5 server
	conn, err := net.Dial("tcp", socksLn.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Handshake
	conn.Write([]byte{Version, 0x01, MethodNoAuth})
	resp := make([]byte, 2)
	io.ReadFull(conn, resp)

	// CONNECT request to target
	targetAddr := targetLn.Addr().(*net.TCPAddr)
	req := []byte{Version, CmdConnect, 0x00, AtypIPv4}
	req = append(req, targetAddr.IP.To4()...)
	req = append(req, byte(targetAddr.Port>>8), byte(targetAddr.Port))
	conn.Write(req)

	// Read reply header
	replyHeader := make([]byte, 4)
	io.ReadFull(conn, replyHeader)
	if replyHeader[1] != RepSuccess {
		t.Fatalf("expected success, got rep=%d", replyHeader[1])
	}

	// Skip bind address
	switch replyHeader[3] {
	case AtypIPv4:
		io.ReadFull(conn, make([]byte, 4+2))
	case AtypIPv6:
		io.ReadFull(conn, make([]byte, 16+2))
	}

	// Test data relay
	testData := []byte("hello socks5")
	conn.Write(testData)

	conn.SetReadDeadline(time.Now().Add(time.Second))
	echoed := make([]byte, len(testData))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	if !bytes.Equal(testData, echoed) {
		t.Errorf("expected %s, got %s", testData, echoed)
	}
}

func TestHandshakeWithAuth(t *testing.T) {
	srv := New(&Config{
		Auth: &AuthConfig{
			User: "admin",
			Pass: "secret123",
		},
	})

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan error)
	go func() {
		done <- srv.handshake(server)
	}()

	// Client sends: VER(5) + NMETHODS(1) + METHODS(UserPass)
	client.Write([]byte{Version, 0x01, MethodUserPass})

	// Read server response (should request UserPass)
	resp := make([]byte, 2)
	if _, err := io.ReadFull(client, resp); err != nil {
		t.Fatalf("read method response: %v", err)
	}
	if resp[0] != Version || resp[1] != MethodUserPass {
		t.Errorf("expected [5, 2], got %v", resp)
	}

	// Send auth: VER(1) + ULEN(5) + "admin" + PLEN(9) + "secret123"
	authReq := []byte{0x01, 5}
	authReq = append(authReq, "admin"...)
	authReq = append(authReq, 9)
	authReq = append(authReq, "secret123"...)
	client.Write(authReq)

	// Read auth response
	authResp := make([]byte, 2)
	if _, err := io.ReadFull(client, authResp); err != nil {
		t.Fatalf("read auth response: %v", err)
	}
	if authResp[0] != 0x01 || authResp[1] != 0x00 {
		t.Errorf("expected [1, 0] (success), got %v", authResp)
	}

	if err := <-done; err != nil {
		t.Errorf("handshake error: %v", err)
	}
}

func TestHandshakeAuthFailure(t *testing.T) {
	srv := New(&Config{
		Auth: &AuthConfig{
			User: "admin",
			Pass: "secret123",
		},
	})

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan error)
	go func() {
		done <- srv.handshake(server)
	}()

	// Client sends: VER(5) + NMETHODS(1) + METHODS(UserPass)
	client.Write([]byte{Version, 0x01, MethodUserPass})

	// Read server response
	resp := make([]byte, 2)
	io.ReadFull(client, resp)

	// Send wrong password
	authReq := []byte{0x01, 5}
	authReq = append(authReq, "admin"...)
	authReq = append(authReq, 5)
	authReq = append(authReq, "wrong"...)
	client.Write(authReq)

	// Read auth response (should fail)
	authResp := make([]byte, 2)
	io.ReadFull(client, authResp)
	if authResp[1] != 0x01 {
		t.Errorf("expected auth failure (status=1), got status=%d", authResp[1])
	}

	err := <-done
	if err == nil {
		t.Error("expected handshake to fail with wrong password")
	}
}

type stubAcceptListener struct {
	accept func() (net.Conn, error)
}

func (l *stubAcceptListener) Accept() (net.Conn, error) { return l.accept() }
func (l *stubAcceptListener) Close() error              { return nil }
func (l *stubAcceptListener) Addr() net.Addr            { return stubAddr("stub") }

type stubAddr string

func (a stubAddr) Network() string { return "tcp" }
func (a stubAddr) String() string  { return string(a) }

type temporaryAcceptError struct{ err error }

func (e temporaryAcceptError) Error() string   { return e.err.Error() }
func (e temporaryAcceptError) Temporary() bool { return true }
func (e temporaryAcceptError) Timeout() bool   { return false }

func TestServeRetriesTemporaryAcceptErrors(t *testing.T) {
	srv := New(&Config{})

	sentinel := errors.New("stop")
	var mu sync.Mutex
	accepts := 0

	ln := &stubAcceptListener{
		accept: func() (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			accepts++
			switch accepts {
			case 1:
				return nil, temporaryAcceptError{err: errors.New("temporary")}
			default:
				return nil, sentinel
			}
		},
	}

	err := srv.Serve(ln)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if accepts != 2 {
		t.Fatalf("expected Serve to retry after temporary error, got %d accept attempts", accepts)
	}
}

func TestResolveMaxConnsDefaultsAndEnv(t *testing.T) {
	t.Setenv("SESSION_PROXY_SOCKS_MAX_CONNS", "")
	if got := resolveMaxConns(0); got != defaultMaxConcurrentConns {
		t.Fatalf("expected default limit %d, got %d", defaultMaxConcurrentConns, got)
	}

	t.Setenv("SESSION_PROXY_SOCKS_MAX_CONNS", "512")
	if got := resolveMaxConns(0); got != 512 {
		t.Fatalf("expected env override 512, got %d", got)
	}

	t.Setenv("SESSION_PROXY_SOCKS_MAX_CONNS", "0")
	if got := resolveMaxConns(0); got != 0 {
		t.Fatalf("expected zero env override to disable limit, got %d", got)
	}

	if got := resolveMaxConns(128); got != 128 {
		t.Fatalf("expected explicit config to win, got %d", got)
	}
}

func TestTryAcquireConnHonorsLimit(t *testing.T) {
	srv := New(&Config{MaxConns: 1})

	if !srv.tryAcquireConn() {
		t.Fatal("expected first acquire to succeed")
	}
	if srv.tryAcquireConn() {
		t.Fatal("expected second acquire to fail at limit")
	}

	srv.releaseConn()
	if !srv.tryAcquireConn() {
		t.Fatal("expected acquire to succeed after release")
	}
	srv.releaseConn()
}

func TestHandshakeNoAuthMethodMismatch(t *testing.T) {
	// Server requires auth, client only supports NoAuth
	srv := New(&Config{
		Auth: &AuthConfig{
			User: "admin",
			Pass: "secret",
		},
	})

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan error)
	go func() {
		done <- srv.handshake(server)
	}()

	// Client sends only NoAuth method
	client.Write([]byte{Version, 0x01, MethodNoAuth})

	// Read server response (should be NoAcceptable)
	resp := make([]byte, 2)
	io.ReadFull(client, resp)
	if resp[1] != MethodNoAcceptable {
		t.Errorf("expected MethodNoAcceptable (0xFF), got %d", resp[1])
	}

	err := <-done
	if err == nil {
		t.Error("expected handshake to fail when client doesn't support required auth")
	}
}

// mockCloseWriter implements net.Conn and CloseWrite().
type mockCloseWriter struct {
	net.Conn
	closeWriteCalled bool
	mu               sync.Mutex
}

func (m *mockCloseWriter) CloseWrite() error {
	m.mu.Lock()
	m.closeWriteCalled = true
	m.mu.Unlock()
	return nil
}

func (m *mockCloseWriter) wasCloseWriteCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeWriteCalled
}

func TestRelayHalfClose_NonTCP(t *testing.T) {
	// Create two net.Pipe pairs — one for client side, one for remote side.
	clientRead, clientWrite := net.Pipe()
	remoteRead, remoteWrite := net.Pipe()

	// Wrap the "remote" write end as a mockCloseWriter.
	remote := &mockCloseWriter{Conn: remoteWrite}

	srv := New(&Config{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.relay(1, "test:80", "127.0.0.1:9999", clientRead, remote)
	}()

	// Write some data from client side, then close client to trigger EOF on client->remote copy.
	clientWrite.Write([]byte("hello"))
	clientWrite.Close()

	// Read the data from remote side to let the copy complete.
	buf := make([]byte, 10)
	remoteRead.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := remoteRead.Read(buf)
	if string(buf[:n]) != "hello" {
		t.Errorf("expected 'hello', got %q", string(buf[:n]))
	}

	// Close remote read side to let the remote->client copy complete.
	remoteRead.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not complete in time")
	}

	if !remote.wasCloseWriteCalled() {
		t.Error("CloseWrite was not called on non-TCP connection implementing the interface")
	}
}
