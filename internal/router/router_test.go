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

func TestEmptyConfig(t *testing.T) {
	r := New(Config{Default: "fallback"})

	// All addresses should go to default
	tests := []string{
		"10.0.0.1:80",
		"example.com:443",
		"192.168.1.1:22",
	}

	for _, addr := range tests {
		got := r.Match(addr)
		if got != "fallback" {
			t.Errorf("Match(%q) = %q, want fallback", addr, got)
		}
	}
}

func TestAddressWithoutPort(t *testing.T) {
	r := New(Config{
		Routes: []struct {
			Match    string
			Upstream string
		}{
			{Match: "10.0.0.0/8", Upstream: "dev"},
		},
		Default: "default",
	})

	// Should work with or without port
	tests := []struct {
		addr     string
		expected string
	}{
		{"10.0.1.5:8080", "dev"},
		{"10.0.1.5", "dev"},
		{"8.8.8.8", "default"},
	}

	for _, tt := range tests {
		got := r.Match(tt.addr)
		if got != tt.expected {
			t.Errorf("Match(%q) = %q, want %q", tt.addr, got, tt.expected)
		}
	}
}

func TestCaseInsensitiveDomain(t *testing.T) {
	r := New(Config{
		Routes: []struct {
			Match    string
			Upstream string
		}{
			{Match: "*.DEV.Internal", Upstream: "dev"},
			{Match: "EXACT.Example.COM", Upstream: "exact"},
		},
		Default: "default",
	})

	tests := []struct {
		addr     string
		expected string
	}{
		{"api.dev.internal:443", "dev"},
		{"API.DEV.INTERNAL:443", "dev"},
		{"exact.example.com:80", "exact"},
		{"EXACT.EXAMPLE.COM:80", "exact"},
	}

	for _, tt := range tests {
		got := r.Match(tt.addr)
		if got != tt.expected {
			t.Errorf("Match(%q) = %q, want %q", tt.addr, got, tt.expected)
		}
	}
}

func TestNoDefault(t *testing.T) {
	r := New(Config{
		Routes: []struct {
			Match    string
			Upstream string
		}{
			{Match: "10.0.0.0/8", Upstream: "dev"},
		},
		// No default set
	})

	// Matched address
	if got := r.Match("10.0.1.5:80"); got != "dev" {
		t.Errorf("Match matched = %q, want dev", got)
	}

	// Unmatched should return empty string
	if got := r.Match("8.8.8.8:53"); got != "" {
		t.Errorf("Match unmatched = %q, want empty", got)
	}
}
