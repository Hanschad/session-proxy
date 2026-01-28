package router

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
)

// DirectConnection is a special value indicating direct connection without proxy.
const DirectConnection = "DIRECT"

// Router matches destination addresses to upstream names.
type Router struct {
	mu          sync.RWMutex
	rules       []rule
	defaultName string
}

type rule struct {
	cidr     *net.IPNet
	domain   string // glob pattern like "*.dev.internal"
	upstream string
}

// Config holds router configuration.
type Config struct {
	Routes []struct {
		Match    string
		Upstream string
	}
	Default string
}

// New creates a new Router from configuration.
func New(cfg Config) (*Router, error) {
	rules, err := parseRules(cfg.Routes)
	if err != nil {
		return nil, err
	}
	return &Router{defaultName: cfg.Default, rules: rules}, nil
}

// Update replaces the router rules with new configuration (hot reload).
func (r *Router) Update(cfg Config) error {
	newRules, err := parseRules(cfg.Routes)
	if err != nil {
		return err
	}

	r.mu.Lock()
	r.rules = newRules
	r.defaultName = cfg.Default
	r.mu.Unlock()
	return nil
}

func parseRules(routes []struct{ Match, Upstream string }) ([]rule, error) {
	var rules []rule
	for _, route := range routes {
		rl := rule{upstream: route.Upstream}

		if strings.Contains(route.Match, "/") {
			_, cidr, err := net.ParseCIDR(route.Match)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q: %w", route.Match, err)
			}
			rl.cidr = cidr
		} else {
			rl.domain = strings.ToLower(route.Match)
		}

		rules = append(rules, rl)
	}
	return rules, nil
}

// Match returns the upstream name for the given address.
// Address can be "host:port" or just "host".
func (r *Router) Match(addr string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	// Try IP matching first
	ip := net.ParseIP(host)
	if ip != nil {
		for _, rl := range r.rules {
			if rl.cidr != nil && rl.cidr.Contains(ip) {
				return rl.upstream
			}
		}
	} else {
		// Domain matching
		hostLower := strings.ToLower(host)
		for _, rl := range r.rules {
			if rl.domain != "" && matchDomain(rl.domain, hostLower) {
				return rl.upstream
			}
		}
	}

	return r.defaultName
}

// matchDomain matches a host against a glob pattern.
// Supports patterns like "*.dev.internal" or "exact.match.com"
func matchDomain(pattern, host string) bool {
	// Use filepath.Match for glob matching
	// Convert "*.dev.internal" style to work properly
	if strings.HasPrefix(pattern, "*.") {
		// Match the suffix
		suffix := pattern[1:] // Remove leading "*"
		return strings.HasSuffix(host, suffix)
	}

	// Try exact match or filepath glob
	matched, _ := filepath.Match(pattern, host)
	return matched
}
