package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hanschad/session-proxy/internal/protocol"
	"github.com/hanschad/session-proxy/internal/session"
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
		log.Println("[INFO] Debug mode enabled")
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
		log.Println("[INFO] Received signal, shutting down...")
		cancel()
	}()

	// Create session manager with automatic reconnection
	mgr := session.NewManager(session.Config{
		InstanceID: *target,
		Region:     *region,
		SSHUser:    *sshUser,
		SSHKeyPath: *sshKey,
		SocksPort:  *socksPort,
	})

	// Run session with automatic reconnection
	if err := mgr.Run(ctx); err != nil {
		if ctx.Err() == nil {
			log.Fatalf("Session failed: %v", err)
		}
	}

	log.Println("[INFO] Shutdown complete")
}
