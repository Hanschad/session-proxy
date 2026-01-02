package proxy

import (
	"context"
	"fmt"
	"net"

	"github.com/armon/go-socks5"
	sshlib "golang.org/x/crypto/ssh"
)

type Server struct {
	port      int
	sshClient *sshlib.Client
	socksSrv  *socks5.Server
}

func NewServer(port int, sshClient *sshlib.Client) (*Server, error) {
	// Create a custom dialer that uses the SSH client
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return sshClient.Dial(network, addr)
	}

	conf := &socks5.Config{
		Dial: dialer,
		// Logger: log.New(os.Stderr, "", log.LstdFlags), // Optional
	}

	srv, err := socks5.New(conf)
	if err != nil {
		return nil, err
	}

	return &Server{
		port:      port,
		sshClient: sshClient,
		socksSrv:  srv,
	}, nil
}

func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	fmt.Printf("SOCKS5 proxy listening on %s\n", addr)
	return s.socksSrv.Serve(ln)
}
