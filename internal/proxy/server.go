package proxy

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/hanschad/session-proxy/internal/config"
	"github.com/hanschad/session-proxy/internal/router"
	"github.com/hanschad/session-proxy/internal/socks5"
	"github.com/hanschad/session-proxy/internal/upstream"
)

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
			d := net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}
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
		if s.pool != nil {
			s.pool.Close()
		}
	}()

	log.Printf("[INFO] SOCKS5 proxy (routing mode) listening on %s", s.listen)
	return s.socksSrv.ServeContext(ctx, ln)
}

func (s *RoutingServer) SOCKSStats() socks5.Stats {
	if s == nil || s.socksSrv == nil {
		return socks5.Stats{}
	}
	return s.socksSrv.Stats()
}
