package proxy

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"sync"

	"github.com/armon/go-socks5"
	sshlib "golang.org/x/crypto/ssh"
)

type Server struct {
	port     int
	socksSrv *socks5.Server
	listener net.Listener

	// Dynamic SSH client reference for transparent reconnection
	sshClient   *sshlib.Client
	sshClientMu sync.RWMutex
}

func NewServer(port int, sshClient *sshlib.Client) (*Server, error) {
	s := &Server{
		port:      port,
		sshClient: sshClient,
	}

	// Create dialer that uses the current SSH client
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		client := s.getSSHClient()
		if client == nil {
			return nil, fmt.Errorf("SSH client not available")
		}
		return client.Dial(network, addr)
	}

	// Custom logger with [ERROR] prefix for go-socks5
	socksLogger := log.New(os.Stderr, "", log.LstdFlags)
	socksLogger.SetPrefix("[ERROR] socks: ")

	conf := &socks5.Config{
		Dial:   dialer,
		Logger: socksLogger,
	}

	srv, err := socks5.New(conf)
	if err != nil {
		return nil, err
	}
	s.socksSrv = srv

	return s, nil
}

// UpdateSSHClient updates the SSH client reference for transparent reconnection.
// Existing connections will fail, but new connections will use the new client.
func (s *Server) UpdateSSHClient(client *sshlib.Client) {
	s.sshClientMu.Lock()
	defer s.sshClientMu.Unlock()
	s.sshClient = client
	log.Printf("[INFO] SOCKS5 dialer updated with new SSH client")
}

// getSSHClient returns the current SSH client (thread-safe)
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
