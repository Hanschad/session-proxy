package socks5

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanschad/session-proxy/internal/trace"
)

var nextConnID uint64
var DebugMode bool

const defaultMaxConcurrentConns = 2048
const clientCloseProbeInterval = 200 * time.Millisecond

func debugLog(format string, args ...interface{}) {
	if DebugMode {
		log.Printf("[DEBUG] "+format, args...)
	}
}

// AuthConfig holds optional authentication credentials.
type AuthConfig struct {
	User string
	Pass string
}

// Config holds server configuration
type Config struct {
	Dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	Auth     *AuthConfig // Optional authentication
	MaxConns int
}

// Stats captures lightweight SOCKS server counters for diagnostics.
type Stats struct {
	ActiveConns           int64  `json:"active_conns"`
	MaxConns              int    `json:"max_conns"`
	AcceptedTotal         uint64 `json:"accepted_total"`
	ConnLimitRejectsTotal uint64 `json:"conn_limit_rejects_total"`
	HandshakeErrorsTotal  uint64 `json:"handshake_errors_total"`
	RequestErrorsTotal    uint64 `json:"request_errors_total"`
	DialErrorsTotal       uint64 `json:"dial_errors_total"`
	ConnectSuccessTotal   uint64 `json:"connect_success_total"`
	RelayClosedTotal      uint64 `json:"relay_closed_total"`
	UnsupportedCmdTotal   uint64 `json:"unsupported_cmd_total"`
}

// Server is a SOCKS5 proxy server
type Server struct {
	dial       func(ctx context.Context, network, addr string) (net.Conn, error)
	auth       *AuthConfig
	maxConns   int
	connTokens chan struct{}
	activeConn atomic.Int64

	acceptedTotal         atomic.Uint64
	connLimitRejectsTotal atomic.Uint64
	handshakeErrorsTotal  atomic.Uint64
	requestErrorsTotal    atomic.Uint64
	dialErrorsTotal       atomic.Uint64
	connectSuccessTotal   atomic.Uint64
	relayClosedTotal      atomic.Uint64
	unsupportedCmdTotal   atomic.Uint64
}

// New creates a new SOCKS5 server
func New(cfg *Config) *Server {
	s := &Server{
		dial:     cfg.Dial,
		auth:     cfg.Auth,
		maxConns: resolveMaxConns(cfg.MaxConns),
	}
	if s.dial == nil {
		s.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	}
	if s.maxConns > 0 {
		s.connTokens = make(chan struct{}, s.maxConns)
	}
	return s
}

// Serve accepts connections from the listener and handles them
func (s *Server) Serve(l net.Listener) error {
	return s.ServeContext(context.Background(), l)
}

// ServeContext accepts connections from the listener and handles them until
// the listener fails or ctx is canceled.
func (s *Server) ServeContext(ctx context.Context, l net.Listener) error {
	if ctx == nil {
		ctx = context.Background()
	}

	var tempDelay time.Duration

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isTemporaryAcceptError(err) {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
					if tempDelay > time.Second {
						tempDelay = time.Second
					}
				}
				log.Printf("[WARN] socks: temporary accept error: %v (retry in %s)", err, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return err
		}
		s.acceptedTotal.Add(1)
		tempDelay = 0
		if !s.tryAcquireConn() {
			s.connLimitRejectsTotal.Add(1)
			log.Printf("[WARN] socks: connection limit reached active=%d max=%d remote=%s", s.activeConn.Load(), s.maxConns, conn.RemoteAddr())
			_ = conn.Close()
			continue
		}
		go func() {
			defer s.releaseConn()
			s.handleConn(ctx, conn)
		}()
	}
}

func (s *Server) Stats() Stats {
	return Stats{
		ActiveConns:           s.activeConn.Load(),
		MaxConns:              s.maxConns,
		AcceptedTotal:         s.acceptedTotal.Load(),
		ConnLimitRejectsTotal: s.connLimitRejectsTotal.Load(),
		HandshakeErrorsTotal:  s.handshakeErrorsTotal.Load(),
		RequestErrorsTotal:    s.requestErrorsTotal.Load(),
		DialErrorsTotal:       s.dialErrorsTotal.Load(),
		ConnectSuccessTotal:   s.connectSuccessTotal.Load(),
		RelayClosedTotal:      s.relayClosedTotal.Load(),
		UnsupportedCmdTotal:   s.unsupportedCmdTotal.Load(),
	}
}

func resolveMaxConns(configured int) int {
	if configured > 0 {
		return configured
	}

	if v := os.Getenv("SESSION_PROXY_SOCKS_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Printf("[WARN] SESSION_PROXY_SOCKS_MAX_CONNS=%q invalid, using default %d", v, defaultMaxConcurrentConns)
			return defaultMaxConcurrentConns
		}
		if n <= 0 {
			return 0
		}
		return n
	}

	return defaultMaxConcurrentConns
}

func (s *Server) tryAcquireConn() bool {
	if s.connTokens == nil {
		s.activeConn.Add(1)
		return true
	}

	select {
	case s.connTokens <- struct{}{}:
		s.activeConn.Add(1)
		return true
	default:
		return false
	}
}

func (s *Server) releaseConn() {
	if s.connTokens != nil {
		select {
		case <-s.connTokens:
		default:
		}
	}
	s.activeConn.Add(-1)
}

func isTemporaryAcceptError(err error) bool {
	type temporary interface {
		Temporary() bool
	}

	if te, ok := err.(temporary); ok && te.Temporary() {
		return true
	}

	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}

	return false
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	connID := atomic.AddUint64(&nextConnID, 1)
	remoteAddr := conn.RemoteAddr().String()
	debugLog("socks: accepted conn=%d remote=%s", connID, remoteAddr)

	// Apply short deadlines for initial negotiation to avoid hanging goroutines.
	// After CONNECT succeeds, we clear deadlines for long-lived streams.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := s.handshake(conn); err != nil {
		s.handshakeErrorsTotal.Add(1)
		log.Printf("[ERROR] socks: handshake failed: conn=%d remote=%s err=%v", connID, remoteAddr, err)
		return
	}

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	req, err := s.readRequest(conn)
	if err != nil {
		s.requestErrorsTotal.Add(1)
		log.Printf("[ERROR] socks: read request failed: conn=%d remote=%s err=%v", connID, remoteAddr, err)
		return
	}

	// Clear deadlines after negotiation.
	_ = conn.SetDeadline(time.Time{})
	clientConn := newBufferedConn(conn)

	switch req.Cmd {
	case CmdConnect:
		s.handleConnect(ctx, clientConn, req, connID, remoteAddr)
	default:
		s.unsupportedCmdTotal.Add(1)
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
	debugLog("socks: authenticated user %q (remote=%s)", string(username), conn.RemoteAddr())
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
func (s *Server) handleConnect(baseCtx context.Context, conn net.Conn, req *Request, connID uint64, clientRemoteAddr string) {
	dialStart := time.Now()
	ctx, cancel, stopWatch := s.newDialContext(baseCtx, conn, 30*time.Second)
	ctx = trace.WithConnID(ctx, connID)
	defer cancel()
	target := req.Addr.String()

	remote, err := s.dial(ctx, "tcp", target)
	stopWatch()
	if err != nil {
		s.dialErrorsTotal.Add(1)
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

	s.connectSuccessTotal.Add(1)
	debugLog("socks: connected conn=%d target=%s remote=%s dial=%s",
		connID, target, clientRemoteAddr, time.Since(dialStart))

	s.relay(connID, target, clientRemoteAddr, conn, remote)
}

// relay copies data between two connections.
// Per-connection transfer stats stay behind debug logging to keep the hot path cheap.
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
		if cw, ok := remote.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		n, err := io.CopyBuffer(client, remote, buf)
		r2c = copyResult{n: n, err: err}
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()

	wg.Wait()

	debugLog("socks: closed conn=%d target=%s remote=%s dur=%s up_bytes=%d down_bytes=%d up_err=%v down_err=%v",
		connID, target, clientRemoteAddr, time.Since(start), c2r.n, r2c.n, c2r.err, r2c.err)
	s.relayClosedTotal.Add(1)
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func newBufferedConn(conn net.Conn) *bufferedConn {
	return &bufferedConn{
		Conn:   conn,
		reader: bufio.NewReader(conn),
	}
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (c *bufferedConn) Peek(n int) ([]byte, error) {
	return c.reader.Peek(n)
}

func (s *Server) newDialContext(baseCtx context.Context, conn net.Conn, timeout time.Duration) (context.Context, context.CancelFunc, context.CancelFunc) {
	if baseCtx == nil {
		baseCtx = context.Background()
	}

	ctx, cancel := context.WithTimeout(baseCtx, timeout)
	stopWatch := func() {}

	peekConn, ok := conn.(interface {
		net.Conn
		Peek(int) ([]byte, error)
	})
	if ok {
		watchCtx, watchCancel := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		stopWatch = func() {
			watchCancel()
			_ = peekConn.SetReadDeadline(time.Now())
			<-watchDone
		}
		go func() {
			defer close(watchDone)
			watchClientDisconnect(watchCtx, peekConn, cancel)
		}()
	}

	return ctx, cancel, stopWatch
}

func watchClientDisconnect(ctx context.Context, conn interface {
	net.Conn
	Peek(int) ([]byte, error)
}, cancel context.CancelFunc) {
	defer func() {
		_ = conn.SetReadDeadline(time.Time{})
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := conn.SetReadDeadline(time.Now().Add(clientCloseProbeInterval)); err != nil {
			return
		}

		_, err := conn.Peek(1)
		if err == nil {
			// Data arrived before CONNECT completed. Keep it buffered for relay and stop probing.
			return
		}

		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			continue
		}

		if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || isConnClosedError(err) {
			cancel()
		}
		return
	}
}

func isConnClosedError(err error) bool {
	if err == nil {
		return false
	}

	var syscallErr syscall.Errno
	if errors.As(err, &syscallErr) {
		switch syscallErr {
		case syscall.ECONNRESET, syscall.EPIPE, syscall.ENOTCONN:
			return true
		}
	}

	msg := err.Error()
	return msg == "use of closed network connection"
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

	// Use syscall errors for reliable cross-platform matching
	var syscallErr syscall.Errno
	if errors.As(err, &syscallErr) {
		switch syscallErr {
		case syscall.ECONNREFUSED:
			rep = RepConnectionRefused
		case syscall.ENETUNREACH:
			rep = RepNetworkUnreachable
		case syscall.EHOSTUNREACH:
			rep = RepHostUnreachable
		}
	}

	s.sendReply(conn, rep, nil)
}
