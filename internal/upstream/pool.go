package upstream

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/hanschad/session-proxy/internal/aws/ssm"
	"github.com/hanschad/session-proxy/internal/config"
	"github.com/hanschad/session-proxy/internal/protocol"
	internalssh "github.com/hanschad/session-proxy/internal/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// Pool manages multiple upstream connections.
type Pool struct {
	groups map[string]*Group
	mu     sync.RWMutex
}

// Group represents a set of instances for a single upstream.
type Group struct {
	name      string
	sshConfig internalssh.Config
	awsCfg    config.AWSConfig
	instances []string
	current   int // Current instance index for failover

	ssmClient *ssm.Client
	adapter   *protocol.Adapter
	sshClient *gossh.Client
	connMu    sync.Mutex

	// For connection maintenance
	ctx    context.Context
	cancel context.CancelFunc
}

// NewPool creates a new upstream pool from configuration.
func NewPool(cfg *config.Config) *Pool {
	p := &Pool{
		groups: make(map[string]*Group),
	}

	for name, up := range cfg.Upstreams {
		p.groups[name] = &Group{
			name: name,
			sshConfig: internalssh.Config{
				User:           up.SSH.User,
				PrivateKeyPath: up.SSH.Key,
			},
			awsCfg:    up.AWS,
			instances: up.Instances,
		}
	}

	return p
}

// Connect establishes connections to all upstreams on startup.
// Returns error if any upstream fails to connect (fail fast).
func (p *Pool) Connect(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for name, group := range p.groups {
		log.Printf("[INFO] Connecting to upstream %q...", name)
		if err := group.connect(ctx); err != nil {
			return fmt.Errorf("upstream %q: %w", name, err)
		}
		log.Printf("[INFO] Upstream %q connected via instance %s", name, group.instances[group.current])

		// Start connection maintenance goroutine
		group.ctx, group.cancel = context.WithCancel(ctx)
		go group.maintain()
	}

	return nil
}

// Dial connects to the specified address through the named upstream.
func (p *Pool) Dial(ctx context.Context, upstreamName, network, addr string) (net.Conn, error) {
	p.mu.RLock()
	group, ok := p.groups[upstreamName]
	p.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("upstream %q not found", upstreamName)
	}

	return group.dial(ctx, network, addr)
}

// dial attempts to connect through the group's SSH client.
func (g *Group) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	g.connMu.Lock()
	defer g.connMu.Unlock()

	if g.sshClient == nil {
		return nil, fmt.Errorf("upstream %s: not connected", g.name)
	}

	conn, err := g.sshClient.Dial(network, addr)
	if err != nil {
		log.Printf("[WARN] upstream %s: dial failed, will reconnect: %v", g.name, err)
		// Mark for reconnection, maintain() will handle it
		return nil, fmt.Errorf("upstream %s: dial failed: %w", g.name, err)
	}

	return conn, nil
}

// connect establishes SSM → SSH connection with failover.
func (g *Group) connect(ctx context.Context) error {
	for attempt := 0; attempt < len(g.instances); attempt++ {
		instance := g.instances[g.current]

		if err := g.connectToInstance(ctx, instance); err != nil {
			log.Printf("[WARN] upstream %s: instance %s failed: %v", g.name, instance, err)
			g.current = (g.current + 1) % len(g.instances)
			continue
		}

		return nil
	}

	return fmt.Errorf("all instances failed")
}

// connectToInstance establishes connection to a specific instance.
func (g *Group) connectToInstance(ctx context.Context, instanceID string) error {
	var err error

	// Create SSM client
	g.ssmClient, err = ssm.NewClient(ctx, ssm.ClientConfig{
		Profile:   g.awsCfg.Profile,
		Region:    g.awsCfg.Region,
		AccessKey: g.awsCfg.AccessKey,
		SecretKey: g.awsCfg.SecretKey,
	})
	if err != nil {
		return fmt.Errorf("create SSM client: %w", err)
	}

	log.Printf("[INFO] upstream %s: starting SSM session to %s (region=%s)...",
		g.name, instanceID, g.ssmClient.Region())

	// Start SSM session
	session, err := g.ssmClient.StartSession(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	// Connect via WebSocket
	g.adapter, err = protocol.NewAdapter(ctx, session.StreamUrl, session.TokenValue)
	if err != nil {
		return fmt.Errorf("websocket connect: %w", err)
	}

	// Wait for SSM handshake
	if err := g.adapter.WaitForHandshake(ctx); err != nil {
		g.adapter.Close()
		g.adapter = nil
		return fmt.Errorf("SSM handshake: %w", err)
	}

	// Establish SSH connection
	g.sshClient, err = internalssh.Connect(g.adapter, g.sshConfig)
	if err != nil {
		g.adapter.Close()
		g.adapter = nil
		return fmt.Errorf("SSH connect: %w", err)
	}

	return nil
}

// maintain monitors connection and reconnects on failure.
func (g *Group) maintain() {
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-time.After(5 * time.Second):
			g.connMu.Lock()
			if g.sshClient == nil || g.adapter == nil {
				log.Printf("[INFO] upstream %s: connection lost, reconnecting...", g.name)
				g.cleanup()
				if err := g.connect(g.ctx); err != nil {
					log.Printf("[ERROR] upstream %s: reconnection failed: %v", g.name, err)
				} else {
					log.Printf("[INFO] upstream %s: reconnected via instance %s", g.name, g.instances[g.current])
				}
			}
			g.connMu.Unlock()
		}
	}
}

// cleanup closes all connections in the group.
func (g *Group) cleanup() {
	if g.sshClient != nil {
		g.sshClient.Close()
		g.sshClient = nil
	}
	if g.adapter != nil {
		g.adapter.Close()
		g.adapter = nil
	}
}

// Close closes all upstream connections.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, g := range p.groups {
		if g.cancel != nil {
			g.cancel()
		}
		g.connMu.Lock()
		g.cleanup()
		g.connMu.Unlock()
	}
}
