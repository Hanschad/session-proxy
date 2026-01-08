package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hanschad/session-proxy/internal/config"
	"github.com/hanschad/session-proxy/internal/protocol"
	"github.com/hanschad/session-proxy/internal/proxy"
	"github.com/hanschad/session-proxy/internal/router"
	"github.com/hanschad/session-proxy/internal/session"
	"github.com/hanschad/session-proxy/internal/upstream"
)

func main() {
	// Config file mode flags
	configPath := flag.String("config", "", "Path to config file (enables multi-upstream mode)")

	// Legacy single-target mode flags
	target := flag.String("target", "", "AWS EC2 Instance ID (legacy single mode)")
	region := flag.String("region", "us-east-1", "AWS Region")
	sshUser := flag.String("ssh-user", "root", "SSH Username")
	sshKey := flag.String("ssh-key", "", "Path to private key (optional if using agent)")
	socksPort := flag.Int("port", 28881, "Local SOCKS5 port")
	awsProfile := flag.String("profile", "", "AWS profile name")
	debug := flag.Bool("debug", false, "Enable debug logging")

	flag.Parse()

	if *debug {
		protocol.DebugMode = true
		log.Println("[INFO] Debug mode enabled")
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

	// Decide mode based on flags
	if *configPath != "" || *target == "" {
		runConfigMode(ctx, *configPath)
	} else {
		runLegacyMode(ctx, *target, *region, *sshUser, *sshKey, *socksPort, *awsProfile)
	}

	log.Println("[INFO] Shutdown complete")
}

// runConfigMode runs with config file (multi-upstream mode)
func runConfigMode(ctx context.Context, configPath string) {
	var cfg *config.Config
	var err error

	if configPath != "" {
		cfg, err = config.Load(configPath)
	} else {
		cfg, err = config.LoadDefault()
	}

	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Build router
	routerCfg := router.Config{Default: cfg.Default}
	for _, r := range cfg.Routes {
		routerCfg.Routes = append(routerCfg.Routes, struct {
			Match    string
			Upstream string
		}{
			Match:    r.Match,
			Upstream: r.Upstream,
		})
	}
	rt := router.New(routerCfg)

	// Build upstream pool
	pool := upstream.NewPool(cfg)

	// Pre-establish connections (fail fast)
	log.Printf("[INFO] Connecting to %d upstreams...", len(cfg.Upstreams))
	if err := pool.Connect(ctx); err != nil {
		log.Fatalf("Failed to connect upstreams: %v", err)
	}
	log.Println("[INFO] All upstreams connected")

	// Create and start server
	server, err := proxy.NewRoutingServer(cfg, rt, pool)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	if err := server.Start(ctx); err != nil {
		if ctx.Err() == nil {
			log.Fatalf("Server error: %v", err)
		}
	}
}

// runLegacyMode runs with CLI flags (single-upstream mode for backward compatibility)
func runLegacyMode(ctx context.Context, target, region, sshUser, sshKey string, socksPort int, awsProfile string) {
	if target == "" {
		fmt.Println("Error: --target is required (or use --config for multi-upstream mode)")
		flag.Usage()
		os.Exit(1)
	}

	mgr := session.NewManager(session.Config{
		InstanceID: target,
		Region:     region,
		SSHUser:    sshUser,
		SSHKeyPath: sshKey,
		SocksPort:  socksPort,
		AWSProfile: awsProfile,
	})

	log.Printf("[INFO] Running in legacy mode: target=%s", target)

	if err := mgr.Run(ctx); err != nil {
		if ctx.Err() == nil {
			log.Fatalf("Session failed: %v", err)
		}
	}
}
