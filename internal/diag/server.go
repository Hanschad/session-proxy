package diag

import (
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"

	"github.com/hanschad/session-proxy/internal/socks5"
	"github.com/hanschad/session-proxy/internal/upstream"
)

type runtimeStats struct {
	Goroutines   int    `json:"goroutines"`
	GOMAXPROCS   int    `json:"gomaxprocs"`
	NumCPU       int    `json:"num_cpu"`
	AllocBytes   uint64 `json:"alloc_bytes"`
	HeapAlloc    uint64 `json:"heap_alloc_bytes"`
	HeapObjects  uint64 `json:"heap_objects"`
	NextGC       uint64 `json:"next_gc_bytes"`
	LastGCUnix   int64  `json:"last_gc_unix_nano"`
	NumGC        uint32 `json:"num_gc"`
	PauseTotalNS uint64 `json:"pause_total_ns"`
	Lookups      uint64 `json:"lookups"`
	Mallocs      uint64 `json:"mallocs"`
	Frees        uint64 `json:"frees"`
}

type stateSnapshot struct {
	Version     string             `json:"version"`
	GeneratedAt time.Time          `json:"generated_at"`
	Runtime     runtimeStats       `json:"runtime"`
	SOCKS       socks5.Stats       `json:"socks"`
	Upstreams   upstream.PoolStats `json:"upstreams"`
}

type stateSource struct {
	version string
	server  interface {
		SOCKSStats() socks5.Stats
	}
	pool interface {
		Stats() upstream.PoolStats
	}
}

// Start launches the diagnostics HTTP server for health checks and debugging.
func Start(ctx context.Context, listen, version string, server interface {
	SOCKSStats() socks5.Stats
}, pool interface {
	Stats() upstream.PoolStats
}) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}

	mux := newHandler(version, server, pool)
	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[WARN] diagnostics shutdown failed: %v", err)
		}
	}()

	go func() {
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[ERROR] diagnostics server failed: %v", err)
		}
	}()

	log.Printf("[INFO] diagnostics listening on %s", ln.Addr())
	return nil
}

func newHandler(version string, server interface {
	SOCKSStats() socks5.Stats
}, pool interface {
	Stats() upstream.PoolStats
}) http.Handler {
	src := stateSource{
		version: version,
		server:  server,
		pool:    pool,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/debug/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(src.snapshot()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.Handle("/debug/vars", expvar.Handler())
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return mux
}

func (s stateSource) snapshot() stateSnapshot {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	var socksStats socks5.Stats
	if s.server != nil {
		socksStats = s.server.SOCKSStats()
	}

	var poolStats upstream.PoolStats
	if s.pool != nil {
		poolStats = s.pool.Stats()
	}

	return stateSnapshot{
		Version:     s.version,
		GeneratedAt: time.Now(),
		Runtime: runtimeStats{
			Goroutines:   runtime.NumGoroutine(),
			GOMAXPROCS:   runtime.GOMAXPROCS(0),
			NumCPU:       runtime.NumCPU(),
			AllocBytes:   mem.Alloc,
			HeapAlloc:    mem.HeapAlloc,
			HeapObjects:  mem.HeapObjects,
			NextGC:       mem.NextGC,
			LastGCUnix:   int64(mem.LastGC),
			NumGC:        mem.NumGC,
			PauseTotalNS: mem.PauseTotalNs,
			Lookups:      mem.Lookups,
			Mallocs:      mem.Mallocs,
			Frees:        mem.Frees,
		},
		SOCKS:     socksStats,
		Upstreams: poolStats,
	}
}
