package diag

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hanschad/session-proxy/internal/socks5"
	"github.com/hanschad/session-proxy/internal/upstream"
)

type fakeServer struct {
	stats socks5.Stats
}

func (f fakeServer) SOCKSStats() socks5.Stats {
	return f.stats
}

type fakePool struct {
	stats upstream.PoolStats
}

func (f fakePool) Stats() upstream.PoolStats {
	return f.stats
}

func TestNewHandlerHealthz(t *testing.T) {
	handler := newHandler("test-version", nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if body := rr.Body.String(); body != "ok\n" {
		t.Fatalf("expected ok body, got %q", body)
	}
}

func TestNewHandlerStateIncludesAppStats(t *testing.T) {
	handler := newHandler("test-version", fakeServer{
		stats: socks5.Stats{
			ActiveConns:         3,
			ConnectSuccessTotal: 9,
		},
	}, fakePool{
		stats: upstream.PoolStats{
			Groups: map[string]upstream.GroupStats{
				"prod": {
					Name:            "prod",
					PoolSize:        4,
					CurrentInstance: "i-123",
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/debug/state", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var snap stateSnapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}

	if snap.Version != "test-version" {
		t.Fatalf("expected version test-version, got %q", snap.Version)
	}
	if snap.SOCKS.ActiveConns != 3 {
		t.Fatalf("expected active conns 3, got %d", snap.SOCKS.ActiveConns)
	}
	if snap.Upstreams.Groups["prod"].CurrentInstance != "i-123" {
		t.Fatalf("expected upstream current instance i-123, got %q", snap.Upstreams.Groups["prod"].CurrentInstance)
	}
	if snap.Runtime.Goroutines <= 0 {
		t.Fatalf("expected goroutine count > 0, got %d", snap.Runtime.Goroutines)
	}
}
