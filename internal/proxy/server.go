package proxy

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/hanschad/session-proxy/internal/config"
	"github.com/hanschad/session-proxy/internal/router"
	"github.com/hanschad/session-proxy/internal/socks5"
	"github.com/hanschad/session-proxy/internal/upstream"
	sshlib "golang.org/x/crypto/ssh"
)

// Server is the legacy single-upstream SOCKS5 server.
type Server struct {
	port     int
	socksSrv *socks5.Server
	listener net.Listener

	sshClient   *sshlib.Client
	sshClientMu sync.RWMutex
}

func NewServer(port int, sshClient *sshlib.Client) (*Server, error) {
	s := &Server{
		port:      port,
		sshClient: sshClient,
	}

	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		client := s.getSSHClient()
		if client == nil {
			return nil, fmt.Errorf("SSH client not available")
		}
		return client.Dial(network, addr)
	}

	s.socksSrv = socks5.New(&socks5.Config{
		Dial: dialer,
	})

	return s, nil
}

func (s *Server) UpdateSSHClient(client *sshlib.Client) {
	s.sshClientMu.Lock()
	defer s.sshClientMu.Unlock()
	s.sshClient = client
	log.Printf("[INFO] SOCKS5 dialer updated with new SSH client")
}

func (s *Server) getSSHClient() *sshlib.Client {
	s.sshClientMu.RLock()
	defer s.sshClientMu.RUnlock()
	return s.sshClient
}

func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	s.listener = ln

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	log.Printf("[INFO] SOCKS5 proxy listening on %s", addr)
	return s.socksSrv.Serve(ln)
}

// RoutingServer is a multi-upstream SOCKS5 server with route-based upstream selection.
type RoutingServer struct {
	listen   string
	socksSrv *socks5.Server
	listener net.Listener
	router   *router.Router
	pool     *upstream.Pool
}

// NewRoutingServer creates a config-driven multi-upstream proxy server.
func NewRoutingServer(cfg *config.Config, r *router.Router, p *upstream.Pool) (*RoutingServer, error) {
	s := &RoutingServer{
		listen: cfg.Listen,
		router: r,
		pool:   p,
	}

	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		upstreamName := s.router.Match(addr)
		// Direct connection when no routes match or explicit DIRECT
		if upstreamName == "" || upstreamName == router.DirectConnection {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
		return s.pool.Dial(ctx, upstreamName, network, addr)
	}

	// Configure SOCKS5 with optional authentication
	socksCfg := &socks5.Config{
		Dial: dialer,
	}

	if cfg.Auth != nil && cfg.Auth.User != "" {
		socksCfg.Auth = &socks5.AuthConfig{
			User: cfg.Auth.User,
			Pass: cfg.Auth.Pass,
		}
		log.Printf("[INFO] SOCKS5 authentication enabled for user %q", cfg.Auth.User)
	}

	s.socksSrv = socks5.New(socksCfg)

	return s, nil
}

// Start begins accepting connections.
func (s *RoutingServer) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.listen, err)
	}
	s.listener = ln

	go func() {
		<-ctx.Done()
		ln.Close()
		s.pool.Close()
	}()

	log.Printf("[INFO] SOCKS5 proxy (routing mode) listening on %s", s.listen)
	return s.socksSrv.Serve(ln)
}
