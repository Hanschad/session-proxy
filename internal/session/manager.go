package session

import (
	"context"
	"log"
	"time"

	"github.com/hanschad/session-proxy/internal/aws/ssm"
	"github.com/hanschad/session-proxy/internal/protocol"
	"github.com/hanschad/session-proxy/internal/proxy"
	internalssh "github.com/hanschad/session-proxy/internal/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// Manager handles SSM session lifecycle with automatic reconnection
type Manager struct {
	instanceID string
	region     string
	sshUser    string
	sshKeyPath string
	socksPort  int

	ssmClient   *ssm.Client
	adapter     *protocol.Adapter
	sshClient   *gossh.Client
	socksServer *proxy.Server

	reconnectInterval time.Duration
	maxRetries        int
}

// Config holds session manager configuration
type Config struct {
	InstanceID string
	Region     string
	SSHUser    string
	SSHKeyPath string
	SocksPort  int
}

// NewManager creates a new session manager
func NewManager(cfg Config) *Manager {
	return &Manager{
		instanceID:        cfg.InstanceID,
		region:            cfg.Region,
		sshUser:           cfg.SSHUser,
		sshKeyPath:        cfg.SSHKeyPath,
		socksPort:         cfg.SocksPort,
		reconnectInterval: 5 * time.Second,
		maxRetries:        10,
	}
}

// Run starts the session with automatic reconnection
func (m *Manager) Run(ctx context.Context) error {
	retries := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := m.connect(ctx); err != nil {
			retries++
			if retries > m.maxRetries {
				log.Printf("[ERROR] Max retries (%d) exceeded, giving up", m.maxRetries)
				return err
			}
			log.Printf("[WARN] Connection failed (attempt %d/%d): %v, retrying in %v",
				retries, m.maxRetries, err, m.reconnectInterval)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.reconnectInterval):
				continue
			}
		}

		// Reset retry counter on successful connection
		retries = 0

		// Wait for connection to close
		<-m.adapter.Done()
		log.Printf("[INFO] Connection closed, reconnecting...")
		m.cleanup()
	}
}

// connect establishes SSM -> SSH -> SOCKS5 pipeline
func (m *Manager) connect(ctx context.Context) error {
	var err error

	// 1. Create SSM client if not exists
	if m.ssmClient == nil {
		m.ssmClient, err = ssm.NewClient(ctx, m.region)
		if err != nil {
			return err
		}
	}

	// 2. Start SSM session
	log.Printf("[INFO] Starting SSM session to %s...", m.instanceID)
	session, err := m.ssmClient.StartSession(ctx, m.instanceID)
	if err != nil {
		return err
	}
	log.Printf("[INFO] Session started (ID: %s)", session.SessionId)

	// 3. Connect via WebSocket
	log.Println("[INFO] Connecting via WebSocket...")
	m.adapter, err = protocol.NewAdapter(ctx, session.StreamUrl, session.TokenValue)
	if err != nil {
		return err
	}

	// 4. Wait for SSM handshake
	log.Println("[INFO] Waiting for SSM handshake...")
	if err := m.adapter.WaitForHandshake(ctx); err != nil {
		m.adapter.Close()
		return err
	}
	log.Println("[INFO] SSM handshake completed")

	// 5. Establish SSH connection
	log.Printf("[INFO] Establishing SSH connection as user '%s'...", m.sshUser)
	m.sshClient, err = internalssh.Connect(m.adapter, internalssh.Config{
		User:           m.sshUser,
		PrivateKeyPath: m.sshKeyPath,
	})
	if err != nil {
		m.adapter.Close()
		return err
	}
	log.Println("[INFO] SSH handshake successful")

	// 6. Start SOCKS5 server or update dialer on reconnection
	if m.socksServer == nil {
		// First connection: create SOCKS5 server
		m.socksServer, err = proxy.NewServer(m.socksPort, m.sshClient)
		if err != nil {
			m.sshClient.Close()
			m.adapter.Close()
			return err
		}

		log.Printf("[INFO] Starting SOCKS5 proxy on port %d...", m.socksPort)
		go func() {
			if err := m.socksServer.Start(ctx); err != nil {
				if ctx.Err() == nil {
					log.Printf("[ERROR] SOCKS5 server error: %v", err)
				}
			}
		}()
	} else {
		// Reconnection: update SSH client reference for transparent failover
		m.socksServer.UpdateSSHClient(m.sshClient)
	}

	return nil
}

// cleanup closes current connections
func (m *Manager) cleanup() {
	if m.sshClient != nil {
		m.sshClient.Close()
		m.sshClient = nil
	}
	if m.adapter != nil {
		m.adapter.Close()
		m.adapter = nil
	}
}
