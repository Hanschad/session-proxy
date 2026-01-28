package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hanschad/session-proxy/internal/config"
	"github.com/hanschad/session-proxy/internal/protocol"
	"github.com/hanschad/session-proxy/internal/proxy"
	"github.com/hanschad/session-proxy/internal/router"
	"github.com/hanschad/session-proxy/internal/ssh"
	"github.com/hanschad/session-proxy/internal/upstream"
)

var version = "dev"

func main() {
	opt := config.New()
	if err := opt.Parse(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		opt.PrintUsage()
		os.Exit(1)
	}

	if opt.ShowVersion {
		fmt.Printf("session-proxy %s\n", version)
		return
	}

	if opt.ShowHelp {
		opt.PrintUsage()
		return
	}

	if opt.IsDebug() {
		protocol.DebugMode = true
		ssh.DebugMode = true
		log.Println("[INFO] Debug mode enabled")
	}

	cfg, err := opt.ToConfig()
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}

	if opt.ShowConfig {
		fmt.Printf("Listen: %s\n", cfg.Listen)
		fmt.Printf("Upstreams: %d\n", len(cfg.Upstreams))
		for name, up := range cfg.Upstreams {
			fmt.Printf("  - %s: %v\n", name, up.Instances)
		}
		fmt.Printf("Routes: %d\n", len(cfg.Routes))
		fmt.Printf("Default: %s\n", cfg.Default)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("[INFO] Received signal, shutting down...")
		cancel()
	}()

	run(ctx, cfg, opt)

	log.Println("[INFO] Shutdown complete")
}

func run(ctx context.Context, cfg *config.Config, opt *config.Options) {
	// Build router
	rt := buildRouter(cfg)

	// Build upstream pool
	pool := upstream.NewPool(cfg)

	// Pre-establish connections
	log.Printf("[INFO] Connecting to %d upstreams...", len(cfg.Upstreams))
	if err := pool.Connect(ctx); err != nil {
		log.Fatalf("Failed to connect upstreams: %v", err)
	}
	log.Println("[INFO] All upstreams connected")

	// Create server
	server, err := proxy.NewRoutingServer(cfg, rt, pool)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	// Start config watcher for hot reload
	if err := opt.Watch(ctx, func(newCfg *config.Config, err error) {
		if err != nil {
			log.Printf("[ERROR] Config reload failed: %v", err)
			return
		}
		onConfigReload(newCfg, rt, pool)
	}); err != nil {
		log.Printf("[WARN] Config watch disabled: %v", err)
	}

	// Start server (blocks until ctx done)
	if err := server.Start(ctx); err != nil {
		if ctx.Err() == nil {
			log.Fatalf("Server error: %v", err)
		}
	}
}

func buildRouter(cfg *config.Config) *router.Router {
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
	rt, err := router.New(routerCfg)
	if err != nil {
		log.Fatalf("Invalid router config: %v", err)
	}
	return rt
}

func onConfigReload(newCfg *config.Config, rt *router.Router, pool *upstream.Pool) {
	// Update routes
	newRouterCfg := router.Config{Default: newCfg.Default}
	for _, r := range newCfg.Routes {
		newRouterCfg.Routes = append(newRouterCfg.Routes, struct {
			Match    string
			Upstream string
		}{
			Match:    r.Match,
			Upstream: r.Upstream,
		})
	}
	if err := rt.Update(newRouterCfg); err != nil {
		log.Printf("[ERROR] Invalid route config, keeping old rules: %v", err)
		return
	}
	log.Printf("[INFO] Routes reloaded: %d rules", len(newCfg.Routes))

	// Note: Pool reconnection is complex, just log for now
	// Full upstream hot-reload would require reconnecting SSH sessions
	log.Printf("[INFO] Config reloaded (upstreams: restart required for changes)")
}
