package socks5

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hanschad/session-proxy/internal/trace"
)

var nextConnID uint64

// AuthConfig holds optional authentication credentials.
type AuthConfig struct {
	User string
	Pass string
}

// Config holds server configuration
type Config struct {
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	Auth *AuthConfig // Optional authentication
}

// Server is a SOCKS5 proxy server
type Server struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	auth *AuthConfig
}

// New creates a new SOCKS5 server
func New(cfg *Config) *Server {
	s := &Server{
		dial: cfg.Dial,
		auth: cfg.Auth,
	}
	if s.dial == nil {
		s.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	}
	return s
}

// Serve accepts connections from the listener and handles them
func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	connID := atomic.AddUint64(&nextConnID, 1)
	remoteAddr := conn.RemoteAddr().String()
	log.Printf("[INFO] socks: accepted conn=%d remote=%s", connID, remoteAddr)

	// Apply short deadlines for initial negotiation to avoid hanging goroutines.
	// After CONNECT succeeds, we clear deadlines for long-lived streams.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := s.handshake(conn); err != nil {
		log.Printf("[ERROR] socks: handshake failed: conn=%d remote=%s err=%v", connID, remoteAddr, err)
		return
	}

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	req, err := s.readRequest(conn)
	if err != nil {
		log.Printf("[ERROR] socks: read request failed: conn=%d remote=%s err=%v", connID, remoteAddr, err)
		return
	}

	// Clear deadlines after negotiation.
	_ = conn.SetDeadline(time.Time{})

	switch req.Cmd {
	case CmdConnect:
		s.handleConnect(conn, req, connID, remoteAddr)
	default:
		s.sendReply(conn, RepCommandNotSupported, nil)
		log.Printf("[WARN] socks: unsupported command conn=%d cmd=%d remote=%s", connID, req.Cmd, remoteAddr)
	}
}

// handshake performs SOCKS5 version/method negotiation
func (s *Server) handshake(conn net.Conn) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read header: %w", err)
	}

	if header[0] != Version {
		return fmt.Errorf("unsupported version: %d", header[0])
	}

	nMethods := int(header[1])
	methods := make([]byte, nMethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return fmt.Errorf("read methods: %w", err)
	}

	// Choose authentication method
	if s.auth != nil && s.auth.User != "" {
		// Require username/password authentication
		hasUserPass := false
		for _, m := range methods {
			if m == MethodUserPass {
				hasUserPass = true
				break
			}
		}

		if !hasUserPass {
			conn.Write([]byte{Version, MethodNoAcceptable})
			return fmt.Errorf("client does not support username/password auth")
		}

		// Request username/password auth
		if _, err := conn.Write([]byte{Version, MethodUserPass}); err != nil {
			return err
		}

		// Perform username/password auth (RFC 1929)
		return s.authenticateUserPass(conn)
	}

	// No auth required
	hasNoAuth := false
	for _, m := range methods {
		if m == MethodNoAuth {
			hasNoAuth = true
			break
		}
	}

	if !hasNoAuth {
		conn.Write([]byte{Version, MethodNoAcceptable})
		return fmt.Errorf("no acceptable auth method")
	}

	_, err := conn.Write([]byte{Version, MethodNoAuth})
	return err
}

// authenticateUserPass performs RFC 1929 username/password authentication
func (s *Server) authenticateUserPass(conn net.Conn) error {
	// RFC 1929 format:
	// +----+------+----------+------+----------+
	// |VER | ULEN |  UNAME   | PLEN |  PASSWD  |
	// +----+------+----------+------+----------+
	// | 1  |  1   | 1 to 255 |  1   | 1 to 255 |
	// +----+------+----------+------+----------+

	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read auth header: %w", err)
	}

	if header[0] != 0x01 { // VER must be 0x01
		s.sendAuthReply(conn, 0x01) // Failure
		return fmt.Errorf("unsupported auth version: %d", header[0])
	}

	ulen := int(header[1])
	if ulen == 0 || ulen > 255 {
		s.sendAuthReply(conn, 0x01)
		return fmt.Errorf("invalid username length: %d", ulen)
	}

	username := make([]byte, ulen)
	if _, err := io.ReadFull(conn, username); err != nil {
		return fmt.Errorf("read username: %w", err)
	}

	plenBuf := make([]byte, 1)
	if _, err := io.ReadFull(conn, plenBuf); err != nil {
		return fmt.Errorf("read password length: %w", err)
	}
	plen := int(plenBuf[0])

	password := make([]byte, plen)
	if plen > 0 {
		if _, err := io.ReadFull(conn, password); err != nil {
			return fmt.Errorf("read password: %w", err)
		}
	}

	// Verify credentials
	if string(username) != s.auth.User || string(password) != s.auth.Pass {
		s.sendAuthReply(conn, 0x01) // Failure
		return fmt.Errorf("authentication failed for user %q", string(username))
	}

	// Success
	s.sendAuthReply(conn, 0x00)
	log.Printf("[INFO] socks: authenticated user %q (remote=%s)", string(username), conn.RemoteAddr())
	return nil
}

// sendAuthReply sends authentication reply
func (s *Server) sendAuthReply(conn net.Conn, status byte) {
	// +----+--------+
	// |VER | STATUS |
	// +----+--------+
	// | 1  |   1    |
	// +----+--------+
	conn.Write([]byte{0x01, status})
}

// Request represents a SOCKS5 request
type Request struct {
	Cmd  byte
	Addr *Addr
}

// readRequest reads a SOCKS5 request
func (s *Server) readRequest(conn net.Conn) (*Request, error) {
	header := make([]byte, 3)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, fmt.Errorf("read request header: %w", err)
	}

	if header[0] != Version {
		return nil, fmt.Errorf("unsupported version: %d", header[0])
	}

	addr, err := ReadAddr(conn)
	if err != nil {
		s.sendReply(conn, RepAddressNotSupported, nil)
		return nil, fmt.Errorf("read address: %w", err)
	}

	return &Request{
		Cmd:  header[1],
		Addr: addr,
	}, nil
}

// handleConnect handles CONNECT command
func (s *Server) handleConnect(conn net.Conn, req *Request, connID uint64, clientRemoteAddr string) {
	dialStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	ctx = trace.WithConnID(ctx, connID)
	defer cancel()
	target := req.Addr.String()

	remote, err := s.dial(ctx, "tcp", target)
	if err != nil {
		log.Printf("[ERROR] socks: dial failed: conn=%d target=%s remote=%s dur=%s err=%q",
			connID, target, clientRemoteAddr, time.Since(dialStart), err)
		s.sendReplyError(conn, err)
		return
	}
	defer remote.Close()

	bindAddr := AddrFromNetAddr(remote.LocalAddr())

	if err := s.sendReply(conn, RepSuccess, bindAddr); err != nil {
		log.Printf("[ERROR] socks: send reply failed: conn=%d target=%s remote=%s err=%v", connID, target, clientRemoteAddr, err)
		return
	}

	log.Printf("[INFO] socks: connected conn=%d target=%s remote=%s dial=%s",
		connID, target, clientRemoteAddr, time.Since(dialStart))

	s.relay(connID, target, clientRemoteAddr, conn, remote)
}

// relay copies data between two connections.
// It logs per-connection transfer stats to aid debugging (e.g. large image uploads).
func (s *Server) relay(connID uint64, target, clientRemoteAddr string, client, remote net.Conn) {
	start := time.Now()

	type copyResult struct {
		n   int64
		err error
	}

	var wg sync.WaitGroup
	wg.Add(2)

	var c2r copyResult // client -> remote
	var r2c copyResult // remote -> client

	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		n, err := io.CopyBuffer(remote, client, buf)
		c2r = copyResult{n: n, err: err}
		if tc, ok := remote.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		n, err := io.CopyBuffer(client, remote, buf)
		r2c = copyResult{n: n, err: err}
		if tc, ok := client.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	wg.Wait()

	log.Printf("[INFO] socks: closed conn=%d target=%s remote=%s dur=%s up_bytes=%d down_bytes=%d up_err=%v down_err=%v",
		connID, target, clientRemoteAddr, time.Since(start), c2r.n, r2c.n, c2r.err, r2c.err)
}

// sendReply sends a SOCKS5 reply
func (s *Server) sendReply(conn net.Conn, rep byte, bind *Addr) error {
	if bind == nil {
		bind = &Addr{Type: AtypIPv4, IP: net.IPv4zero, Port: 0}
	}

	reply := []byte{Version, rep, 0x00}
	if _, err := conn.Write(reply); err != nil {
		return err
	}
	_, err := bind.WriteTo(conn)
	return err
}

// sendReplyError maps an error to the appropriate reply code
func (s *Server) sendReplyError(conn net.Conn, err error) {
	var rep byte = RepGeneralFailure

	if netErr, ok := err.(net.Error); ok {
		if netErr.Timeout() {
			rep = RepTTLExpired
		}
	}

	if opErr, ok := err.(*net.OpError); ok {
		switch opErr.Err.Error() {
		case "connection refused":
			rep = RepConnectionRefused
		case "network is unreachable":
			rep = RepNetworkUnreachable
		case "no route to host":
			rep = RepHostUnreachable
		}
	}

	s.sendReply(conn, rep, nil)
}
