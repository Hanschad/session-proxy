package proxy

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/hanschad/session-proxy/internal/socks5"
	sshlib "golang.org/x/crypto/ssh"
)

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
