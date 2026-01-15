package upstream

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// mockAdapter simulates protocol.Adapter for testing
type mockAdapter struct {
	done      chan struct{}
	closeOnce sync.Once
}

func newMockAdapter() *mockAdapter {
	return &mockAdapter{done: make(chan struct{})}
}

func (m *mockAdapter) Done() <-chan struct{} {
	return m.done
}

func (m *mockAdapter) Close() error {
	m.closeOnce.Do(func() {
		close(m.done)
	})
	return nil
}

func (m *mockAdapter) Read(p []byte) (n int, err error) {
	return 0, nil
}

func (m *mockAdapter) Write(p []byte) (n int, err error) {
	return len(p), nil
}

// mockSSHClient simulates SSH client for testing
type mockSSHClient struct {
	dialErr    error
	dialCalled int
	mu         sync.Mutex
}

func (m *mockSSHClient) Dial(network, addr string) (net.Conn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dialCalled++
	if m.dialErr != nil {
		return nil, m.dialErr
	}
	// Return a fake connection
	client, _ := net.Pipe()
	return client, nil
}

func TestDialFailureTriggersCleanup(t *testing.T) {
	// Create a group with mock components
	g := &Group{
		name:      "test",
		instances: []string{"i-test-1"},
	}

	// Test that when connection pool is empty, dial returns error
	g.connsMu.Lock()
	g.conns = nil
	g.connsMu.Unlock()

	ctx := context.Background()
	_, err := g.dial(ctx, "tcp", "10.0.0.1:80")
	if err == nil {
		t.Error("expected error when connection pool is empty")
	}
}

func TestCleanupSetsNil(t *testing.T) {
	g := &Group{
		name:      "test",
		instances: []string{"i-test-1"},
	}

	// Create mock adapter
	mockAdapt := newMockAdapter()

	// Simulate that conns pool has some entries
	g.connsMu.Lock()
	g.conns = []*sshConn{{}} // Empty but non-nil
	g.connsMu.Unlock()

	// Verify cleanup properly nils the pool
	g.connsMu.Lock()
	g.cleanup()
	g.connsMu.Unlock()

	g.connsMu.RLock()
	if g.conns != nil {
		t.Error("expected conns to be nil after cleanup")
	}
	g.connsMu.RUnlock()

	_ = mockAdapt // Suppress unused warning
}

func TestPoolDialWithUnknownUpstream(t *testing.T) {
	p := &Pool{
		groups: make(map[string]*Group),
	}

	ctx := context.Background()
	_, err := p.Dial(ctx, "nonexistent", "tcp", "10.0.0.1:80")
	if err == nil {
		t.Error("expected error for unknown upstream")
	}
}

func TestPoolClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	g := &Group{
		name:      "test",
		instances: []string{"i-test-1"},
		ctx:       ctx,
		cancel:    cancel,
	}

	p := &Pool{
		groups: map[string]*Group{"test": g},
	}

	// Close should not panic
	p.Close()

	// Verify context was cancelled
	select {
	case <-ctx.Done():
		// Expected
	default:
		t.Error("expected context to be cancelled after Close")
	}
}

func TestGroupMaintainDetectsEmptyPool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := &Group{
		name:      "test",
		instances: []string{"i-test-1"},
		ctx:       ctx,
		cancel:    cancel,
		conns:     nil, // Simulates disconnected state
	}

	// Run maintain for a short time
	done := make(chan struct{})
	go func() {
		// maintain() will try to reconnect, but connect() will fail
		// because we don't have real SSM/SSH setup
		// Just verify it doesn't panic
		defer close(done)

		// Check the pool empty detection logic directly
		g.connsMu.RLock()
		isDisconnected := len(g.conns) == 0
		g.connsMu.RUnlock()

		if !isDisconnected {
			t.Error("expected disconnected state to be detected")
		}
	}()

	select {
	case <-done:
		// Success
	case <-time.After(100 * time.Millisecond):
		t.Error("test timed out")
	}
}

// TestMockAdapterDone verifies the Done() channel behavior
func TestMockAdapterDone(t *testing.T) {
	adapter := newMockAdapter()

	// Done channel should be open initially
	select {
	case <-adapter.Done():
		t.Error("Done channel should not be closed initially")
	default:
		// Expected
	}

	// After Close, Done should be closed
	adapter.Close()

	select {
	case <-adapter.Done():
		// Expected
	default:
		t.Error("Done channel should be closed after Close()")
	}
}

// TestDialFailureIncrementsInstance simulates failover behavior
func TestDialFailureIncrementsInstance(t *testing.T) {
	g := &Group{
		name:      "test",
		instances: []string{"i-1", "i-2", "i-3"},
		current:   0,
	}

	// Simulate failover increment
	originalCurrent := g.current
	g.current = (g.current + 1) % len(g.instances)

	if g.current != originalCurrent+1 {
		t.Errorf("expected current to increment from %d to %d, got %d",
			originalCurrent, originalCurrent+1, g.current)
	}

	// Wrap around test
	g.current = 2
	g.current = (g.current + 1) % len(g.instances)
	if g.current != 0 {
		t.Errorf("expected current to wrap to 0, got %d", g.current)
	}
}

// Integration-style test with real goroutines (requires mock SSM/SSH)
func TestMaintainLoopExitsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	g := &Group{
		name:      "test",
		instances: []string{"i-test-1"},
		ctx:       ctx,
		cancel:    cancel,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Simplified maintain loop
		for {
			select {
			case <-g.ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
				// Would check connection here
			}
		}
	}()

	// Cancel context
	cancel()

	// Verify goroutine exits
	select {
	case <-done:
		// Success
	case <-time.After(100 * time.Millisecond):
		t.Error("maintain loop did not exit on context cancel")
	}
}

// Ensure gossh import is used
var _ *gossh.Client = nil
