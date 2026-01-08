package router

import (
	"testing"
)

func TestCIDRMatching(t *testing.T) {
	r := New(Config{
		Routes: []struct {
			Match    string
			Upstream string
		}{
			{Match: "10.0.0.0/8", Upstream: "dev"},
			{Match: "192.168.0.0/16", Upstream: "home"},
		},
		Default: "default",
	})

	tests := []struct {
		addr     string
		expected string
	}{
		{"10.0.1.5:80", "dev"},
		{"10.255.255.255:443", "dev"},
		{"192.168.1.100:22", "home"},
		{"8.8.8.8:53", "default"},
		{"172.16.0.1:80", "default"},
	}

	for _, tt := range tests {
		got := r.Match(tt.addr)
		if got != tt.expected {
			t.Errorf("Match(%q) = %q, want %q", tt.addr, got, tt.expected)
		}
	}
}

func TestDomainMatching(t *testing.T) {
	r := New(Config{
		Routes: []struct {
			Match    string
			Upstream string
		}{
			{Match: "*.dev.internal", Upstream: "dev"},
			{Match: "*.uat.internal", Upstream: "uat"},
			{Match: "exact.example.com", Upstream: "exact"},
		},
		Default: "fallback",
	})

	tests := []struct {
		addr     string
		expected string
	}{
		{"api.dev.internal:443", "dev"},
		{"db.dev.internal:5432", "dev"},
		{"service.uat.internal:8080", "uat"},
		{"exact.example.com:80", "exact"},
		{"other.example.com:80", "fallback"},
		{"google.com:443", "fallback"},
	}

	for _, tt := range tests {
		got := r.Match(tt.addr)
		if got != tt.expected {
			t.Errorf("Match(%q) = %q, want %q", tt.addr, got, tt.expected)
		}
	}
}

func TestMixedMatching(t *testing.T) {
	r := New(Config{
		Routes: []struct {
			Match    string
			Upstream string
		}{
			{Match: "10.0.0.0/8", Upstream: "dev-ip"},
			{Match: "*.dev.internal", Upstream: "dev-dns"},
		},
		Default: "default",
	})

	tests := []struct {
		addr     string
		expected string
	}{
		{"10.0.1.5:80", "dev-ip"},
		{"api.dev.internal:443", "dev-dns"},
		{"external.com:80", "default"},
	}

	for _, tt := range tests {
		got := r.Match(tt.addr)
		if got != tt.expected {
			t.Errorf("Match(%q) = %q, want %q", tt.addr, got, tt.expected)
		}
	}
}
