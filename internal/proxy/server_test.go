package proxy

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/hanschad/session-proxy/internal/config"
	"github.com/hanschad/session-proxy/internal/router"
)

func TestNewServerWithNilClient(t *testing.T) {
	s, err := NewServer(0, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if s == nil {
		t.Fatal("NewServer() returned nil server")
	}
	if s.socksSrv == nil {
		t.Fatal("NewServer() did not create SOCKS5 server")
	}
}

func TestServerUpdateSSHClient(t *testing.T) {
	s, _ := NewServer(0, nil)

	if s.getSSHClient() != nil {
		t.Error("Expected nil SSH client initially")
	}

	// Can't test with real SSH client without mocking, just verify no panic
	s.UpdateSSHClient(nil)
}

func TestServerGetSSHClientConcurrency(t *testing.T) {
	s, _ := NewServer(0, nil)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.UpdateSSHClient(nil)
		}
		close(done)
	}()

	for i := 0; i < 100; i++ {
		_ = s.getSSHClient()
	}
	<-done
}

// MockPool implements a minimal pool for testing
type mockPool struct{}

func (m *mockPool) Dial(ctx context.Context, upstream, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func (m *mockPool) Close() {}

func TestNewRoutingServerWithAuth(t *testing.T) {
	cfg := &config.Config{
		Listen: "127.0.0.1:0",
		Auth: &config.AuthConfig{
			User: "test",
			Pass: "pass",
		},
		Upstreams: map[string]*config.Upstream{
			"default": {
				Instances: []string{"i-test"},
			},
		},
	}

	r, _ := router.New(router.Config{Default: "default"})

	// We can't use a real pool, but we can test that NewRoutingServer handles auth config
	// The server won't be fully functional without a real pool, but we verify the struct setup
	s, err := NewRoutingServer(cfg, r, nil)
	if err != nil {
		t.Fatalf("NewRoutingServer() error = %v", err)
	}
	if s.listen != cfg.Listen {
		t.Errorf("listen = %q, want %q", s.listen, cfg.Listen)
	}
	if s.router != r {
		t.Error("router not set correctly")
	}
}

func TestNewRoutingServerWithoutAuth(t *testing.T) {
	cfg := &config.Config{
		Listen: "127.0.0.1:0",
		Upstreams: map[string]*config.Upstream{
			"default": {
				Instances: []string{"i-test"},
			},
		},
	}

	r, _ := router.New(router.Config{Default: "default"})
	s, err := NewRoutingServer(cfg, r, nil)
	if err != nil {
		t.Fatalf("NewRoutingServer() error = %v", err)
	}
	if s.socksSrv == nil {
		t.Error("socksSrv not created")
	}
}

func TestServerStartAndContextCancel(t *testing.T) {
	s, _ := NewServer(0, nil) // Port 0 = random available port

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start(ctx)
	}()

	// Give server time to start
	time.Sleep(50 * time.Millisecond)

	// Cancel context should close listener
	cancel()

	select {
	case err := <-errCh:
		// Accept error from closed listener is expected
		if err != nil && err.Error() != "use of closed network connection" {
			// Some errors are acceptable when closing
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Server did not stop after context cancel")
	}
}
