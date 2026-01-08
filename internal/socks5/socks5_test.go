package socks5

import (
	"bytes"
	"context"
	"io"
	"net"
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
