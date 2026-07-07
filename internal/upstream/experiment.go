package upstream

import (
	"fmt"

	"github.com/hanschad/session-proxy/internal/protocol"
	gossh "golang.org/x/crypto/ssh"
)

// ExperimentSSHSession returns the SSH client and SSM adapter for the first
// connection in an upstream group. Intended for resume validation tooling only.
func (p *Pool) ExperimentSSHSession(upstreamName string) (*gossh.Client, *protocol.Adapter, error) {
	p.mu.RLock()
	group, ok := p.groups[upstreamName]
	p.mu.RUnlock()
	if !ok {
		return nil, nil, fmt.Errorf("upstream %q not found", upstreamName)
	}

	group.connsMu.RLock()
	defer group.connsMu.RUnlock()
	if len(group.conns) == 0 {
		return nil, nil, fmt.Errorf("upstream %q has no connections", upstreamName)
	}
	sc := group.conns[0]
	if sc.sshClient == nil || sc.adapter == nil {
		return nil, nil, fmt.Errorf("upstream %q connection is not ready", upstreamName)
	}
	return sc.sshClient, sc.adapter, nil
}
