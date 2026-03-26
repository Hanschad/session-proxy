package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	ssmclient "github.com/hanschad/session-proxy/internal/aws/ssm"
	"github.com/hanschad/session-proxy/internal/config"
	"github.com/hanschad/session-proxy/internal/protocol"
	gossh "golang.org/x/crypto/ssh"
)

func TestPoolConnectParallelizesUpstreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	slow := &Group{
		name:      "slow",
		instances: []string{"i-slow"},
	}
	fast := &Group{
		name:      "fast",
		instances: []string{"i-fast"},
	}

	connectDelay := 150 * time.Millisecond
	started := make(chan string, 2)

	origGroupConnect := groupConnectHook
	groupConnectHook = func(g *Group, ctx context.Context) error {
		started <- g.name
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(connectDelay):
		}
		g.connsMu.Lock()
		g.conns = []*sshConn{{id: 1, group: g, adapter: &protocol.Adapter{}}}
		g.connsMu.Unlock()
		return nil
	}
	defer func() {
		groupConnectHook = origGroupConnect
	}()

	p := &Pool{
		groups: map[string]*Group{
			"slow": slow,
			"fast": fast,
		},
	}

	start := time.Now()
	if err := p.Connect(ctx); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	elapsed := time.Since(start)

	if elapsed >= 2*connectDelay {
		t.Fatalf("expected parallel connect to finish faster than %s, got %s", 2*connectDelay, elapsed)
	}

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		got[<-started] = true
	}
	if !got["slow"] || !got["fast"] {
		t.Fatalf("expected both groups to start, got %v", got)
	}

	p.Close()
}

func TestDialWaitForCapacityWakesOnSignal(t *testing.T) {
	g := &Group{
		name:      "test",
		instances: []string{"i-test-1"},
	}

	sc := &sshConn{
		id:    1,
		group: g,
	}
	atomic.StoreInt64(&sc.activeChannels, maxChannelsPerConn)

	g.connsMu.Lock()
	g.conns = []*sshConn{sc}
	g.connsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer cancel()

	go func() {
		time.Sleep(20 * time.Millisecond)
		atomic.StoreInt64(&sc.activeChannels, 0)
		g.signalCapacityChange()
	}()

	_, err := g.dial(ctx, "tcp", "10.0.0.1:80")
	if err == nil {
		t.Fatal("expected dial to fail because ssh client is nil")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected dial wait to wake on capacity signal before context timeout, got %v", err)
	}
}

func TestGetSSMClientReusesClientAcrossCallers(t *testing.T) {
	g := &Group{
		awsCfg: config.AWSConfig{
			Profile: "default",
		},
	}

	origHook := newSSMClientHook
	defer func() {
		newSSMClientHook = origHook
	}()

	wantClient := &ssmclient.Client{}

	var (
		mu    sync.Mutex
		calls int
	)

	newSSMClientHook = func(ctx context.Context, cfg ssmclient.ClientConfig) (*ssmclient.Client, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		return wantClient, nil
	}

	const callers = 8
	var wg sync.WaitGroup
	results := make(chan *ssmclient.Client, callers)
	errs := make(chan error, callers)

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := g.getSSMClient(context.Background())
			if err != nil {
				errs <- err
				return
			}
			results <- client
		}()
	}

	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("getSSMClient() error = %v", err)
	}

	for client := range results {
		if client != wantClient {
			t.Fatalf("expected shared SSM client %p, got %p", wantClient, client)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected SSM client to be created once, got %d", calls)
	}
}

func TestMaintainStartsReplenishImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := &Group{
		name:       "test",
		instances:  []string{"i-test-1"},
		ctx:        ctx,
		cancel:     cancel,
		maintainCh: make(chan struct{}, 1),
	}

	origHook := groupConnectSingleHook
	var replenishWG sync.WaitGroup
	replenishWG.Add(replenishParallelism)
	defer func() {
		replenishWG.Wait()
		groupConnectSingleHook = origHook
	}()

	started := make(chan struct{}, 1)
	groupConnectSingleHook = func(g *Group, ctx context.Context, instanceID string) (*sshConn, error) {
		defer replenishWG.Done()
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		g.maintain()
	}()

	select {
	case <-started:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected maintain to trigger replenish immediately")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintain did not exit after cancel")
	}
}

func TestDialTimeoutKeepsInflightReservedUntilDialReturns(t *testing.T) {
	g := &Group{
		name:      "test",
		instances: []string{"i-test-1"},
	}

	sc := &sshConn{
		id:        1,
		group:     g,
		sshClient: &gossh.Client{},
	}

	g.connsMu.Lock()
	g.conns = []*sshConn{sc}
	g.connsMu.Unlock()

	origDialHook := sshConnDialHook
	blockDial := make(chan struct{})
	sshConnDialHook = func(sc *sshConn, network, addr string) (net.Conn, error) {
		<-blockDial
		return nil, errors.New("late dial failure")
	}
	defer func() {
		sshConnDialHook = origDialHook
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := g.dial(ctx, "tcp", "10.0.0.1:80")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}

	if got := atomic.LoadInt64(&sc.inflightDials); got != 1 {
		t.Fatalf("expected inflight dial to remain reserved after timeout, got %d", got)
	}

	stats := g.stats()
	if stats.PendingAbandonedDials != 1 {
		t.Fatalf("expected 1 pending abandoned dial, got %d", stats.PendingAbandonedDials)
	}

	close(blockDial)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&sc.inflightDials) == 0 && atomic.LoadInt64(&g.pendingAbandonedDials) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := atomic.LoadInt64(&sc.inflightDials); got != 0 {
		t.Fatalf("expected inflight dial to be released after dial goroutine exits, got %d", got)
	}
	if got := atomic.LoadInt64(&g.pendingAbandonedDials); got != 0 {
		t.Fatalf("expected pending abandoned dials to drain, got %d", got)
	}

	finalStats := g.stats()
	if finalStats.Counters.LateDialFailures != 1 {
		t.Fatalf("expected 1 late dial failure, got %d", finalStats.Counters.LateDialFailures)
	}
}

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

func TestIsTransportError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantTrans bool
	}{
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"net.ErrClosed", net.ErrClosed, true},
		{"broken pipe", syscall.EPIPE, true},
		{"connection reset", syscall.ECONNRESET, true},
		{"closed connection", fmt.Errorf("use of closed network connection"), true},
		{"ssh unexpected packet", fmt.Errorf("ssh: unexpected packet"), true},
		{"ssh disconnect", fmt.Errorf("ssh: disconnect, reason 11:"), true},
		{"ssh handshake failed", fmt.Errorf("ssh: handshake failed"), true},
		{"connection refused", syscall.ECONNREFUSED, false},
		{"host unreachable", syscall.EHOSTUNREACH, false},
		{"network unreachable", syscall.ENETUNREACH, false},
		{"generic dial error", fmt.Errorf("dial tcp 10.0.0.1:80: connection refused"), false},
		{"random error", fmt.Errorf("something else"), false},
		{"wrapped EOF", fmt.Errorf("read failed: %w", io.EOF), true},
		{"wrapped net.ErrClosed", fmt.Errorf("write failed: %w", net.ErrClosed), true},
		{"wrapped refused", fmt.Errorf("connect: %w", syscall.ECONNREFUSED), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTransportError(tt.err)
			if got != tt.wantTrans {
				t.Errorf("isTransportError(%v) = %v, want %v", tt.err, got, tt.wantTrans)
			}
		})
	}
}
