package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hanschad/session-proxy/internal/aws/ssm"
	"github.com/hanschad/session-proxy/internal/protocol"
	"github.com/hanschad/session-proxy/internal/proxy"
	"github.com/hanschad/session-proxy/internal/ssh"
)

func main() {
	target := flag.String("target", "", "AWS EC2 Instance ID (e.g., i-xxx)")
	region := flag.String("region", "us-east-1", "AWS Region")
	sshUser := flag.String("ssh-user", "root", "SSH Username") // Default from user request was root
	sshKey := flag.String("ssh-key", "", "Path to private key (optional if using agent)")
	socksPort := flag.Int("port", 28881, "Local SOCKS5 port")
	debug := flag.Bool("debug", false, "Enable debug logging")

	flag.Parse()

	if *debug {
		protocol.DebugMode = true
		log.Println("Debug mode enabled")
	}

	if *target == "" {
		fmt.Println("Error: --target is required")
		flag.Usage()
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Received signal, shutting down...")
		cancel()
	}()

	// 1. Start SSM Session
	log.Printf("Starting SSM session to %s...", *target)
	ssmClient, err := ssm.NewClient(ctx, *region)
	if err != nil {
		log.Fatalf("Failed to create SSM client: %v", err)
	}

	session, err := ssmClient.StartSession(ctx, *target)
	if err != nil {
		log.Fatalf("Failed to start session: %v", err)
	}
	log.Printf("Session started (ID: %s)", session.SessionId)

	// 2. Connect via WebSocket Adapter
	log.Println("Connecting via WebSocket...")
	adapter, err := protocol.NewAdapter(ctx, session.StreamUrl, session.TokenValue)
	if err != nil {
		log.Fatalf("Failed to connect to stream URL: %v", err)
	}
	// Important: Handle cancellation by closing the adapter to unblock Read() calls
	go func() {
		<-ctx.Done()
		adapter.Close()
	}()
	// We still defer Close in case of normal exit, though Close is idempotent-ish
	defer adapter.Close()

	// 3. Establish SSH Connection
	log.Printf("Establishing SSH connection as user '%s'...", *sshUser)
	sshClient, err := ssh.Connect(adapter, ssh.Config{
		User:           *sshUser,
		PrivateKeyPath: *sshKey,
	})
	if err != nil {
		log.Fatalf("SSH connection failed: %v", err)
	}
	defer sshClient.Close()
	log.Println("SSH handshake successful")

	// 4. Start SOCKS5 Server
	socksServer, err := proxy.NewServer(*socksPort, sshClient)
	if err != nil {
		log.Fatalf("Failed to create SOCKS5 server: %v", err)
	}

	log.Printf("Starting SOCKS5 proxy on port %d...", *socksPort)
	if err := socksServer.Start(ctx); err != nil {
		// Start returns error when server stops usually
		if ctx.Err() == nil {
			log.Fatalf("SOCKS5 server failed: %v", err)
		}
	}

	log.Println("Shutdown complete")
}
