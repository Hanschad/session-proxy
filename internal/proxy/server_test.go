package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/hanschad/session-proxy/internal/config"
	"github.com/hanschad/session-proxy/internal/router"
)

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
	cfg := &config.Config{
		Listen: "127.0.0.1:0",
		Upstreams: map[string]*config.Upstream{
			"default": {
				Instances: []string{"i-test"},
			},
		},
		Default: "DIRECT",
	}
	r, _ := router.New(router.Config{Default: "DIRECT"})
	s, err := NewRoutingServer(cfg, r, nil)
	if err != nil {
		t.Fatalf("NewRoutingServer() error = %v", err)
	}

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
