package router

import (
	"net"
	"path/filepath"
	"strings"
)

// Router matches destination addresses to upstream names.
type Router struct {
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
func New(cfg Config) *Router {
	r := &Router{defaultName: cfg.Default}

	for _, route := range cfg.Routes {
		rl := rule{upstream: route.Upstream}

		// Try to parse as CIDR
		_, cidr, err := net.ParseCIDR(route.Match)
		if err == nil {
			rl.cidr = cidr
		} else {
			// Treat as domain glob pattern
			rl.domain = strings.ToLower(route.Match)
		}

		r.rules = append(r.rules, rl)
	}

	return r
}

// Match returns the upstream name for the given address.
// Address can be "host:port" or just "host".
func (r *Router) Match(addr string) string {
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
