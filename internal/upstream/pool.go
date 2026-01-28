package upstream

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hanschad/session-proxy/internal/aws/ssm"
	"github.com/hanschad/session-proxy/internal/config"
	"github.com/hanschad/session-proxy/internal/protocol"
	"github.com/hanschad/session-proxy/internal/retry"
	internalssh "github.com/hanschad/session-proxy/internal/ssh"
	"github.com/hanschad/session-proxy/internal/trace"
	gossh "golang.org/x/crypto/ssh"
)

func debugLog(format string, args ...interface{}) {
	if protocol.DebugMode {
		log.Printf("[DEBUG] "+format, args...)
	}
}

var nextSSHConnID uint64

// Pool manages multiple upstream connections.
type Pool struct {
	groups map[string]*Group
	mu     sync.RWMutex
}

// sshConn represents a single SSH connection through SSM.
type sshConn struct {
	id uint64

	ssmClient *ssm.Client
	adapter   *protocol.Adapter
	sshClient *gossh.Client

	activeChannels int64 // Number of active channels (approx via conn lifetime)
	inflightDials  int64 // Number of in-flight Dial() calls

	// Per-connection dial timeout tracking.
	dialTimeoutCount        int64
	lastDialTimeoutUnixNano int64

	draining int32 // atomic: 1 = draining (no new channels), 0 = normal
}

func (sc *sshConn) markDraining() {
	atomic.StoreInt32(&sc.draining, 1)
}

func (sc *sshConn) isDraining() bool {
	return atomic.LoadInt32(&sc.draining) == 1
}

func (sc *sshConn) isIdle() bool {
	return atomic.LoadInt64(&sc.activeChannels) == 0 && atomic.LoadInt64(&sc.inflightDials) == 0
}

func (sc *sshConn) close() {
	if sc.sshClient != nil {
		sc.sshClient.Close()
	}
	if sc.adapter != nil {
		sc.adapter.Close()
	}
}

// closeDrainingConns waits for each connection to become idle (or timeout) and closes it.
func (g *Group) closeDrainingConns(conns []*sshConn, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for _, sc := range conns {
		for time.Now().Before(deadline) {
			if sc.isIdle() {
				break
			}
			time.Sleep(1 * time.Second)
		}
		log.Printf("[INFO] upstream %s: closing draining connection (sshConn=%d, idle=%v)",
			g.name, sc.id, sc.isIdle())
		sc.close()
	}
}

// Default pool size per upstream. Having multiple SSH connections prevents
// channel contention when one connection is busy with large data transfers.
const (
	defaultPoolSize = 4

	// maxPoolSize bounds how far we can scale out the pool under load.
	// Larger values reduce head-of-line blocking at the cost of more SSM+SSH sessions.
	// NOTE: Too many concurrent sessions can cause the remote side to close channels.
	maxPoolSize = 12

	// maxChannelsPerConn limits multiplexing on a single SSH connection.
	// Keeping this >1 reduces SSM session churn for workloads like docker push (many parallel HTTP connections).
	maxChannelsPerConn = 4

	// scaleParallelism limits how many new SSM+SSH sessions we try to establish concurrently
	// when scaling under load.
	scaleParallelism = 2

	// replenishParallelism limits concurrent replenish attempts when pool is below target size.
	replenishParallelism = 2

	// replenishTimeout bounds a single replenish attempt to avoid blocking maintenance.
	replenishTimeout = 60 * time.Second
)

type trackedConn struct {
	net.Conn
	sc   *sshConn
	once sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		atomic.AddInt64(&c.sc.activeChannels, -1)
	})
	return err
}

// Group represents a set of instances for a single upstream.
type Group struct {
	name       string
	sshConfig  internalssh.Config
	awsCfg     config.AWSConfig
	instances  []string
	current    int // Current instance index for failover
	instanceMu sync.Mutex

	// Pool of SSH connections for parallelism.
	conns    []*sshConn
	connsMu  sync.RWMutex
	nextConn uint64 // Round-robin counter

	// Serialize pool growth calculations and bound concurrent SSM+SSH session creation.
	scaleMu       sync.Mutex
	scaleInFlight int

	// Forced reconnect throttling.
	lastForcedReconnectUnixNano int64
	forcedReconnectInProgress   int32
	// Replenish backoff and concurrency control.
	replenishMu          sync.Mutex
	replenishInFlight    int
	replenishFailures    int
	replenishNextAttempt time.Time
	replenishRetryer     *retry.ExponentialRetryer
	replenishAttemptID   uint64

	// For connection maintenance
	ctx                     context.Context
	cancel                  context.CancelFunc
	lastMaintainTime        time.Time     // Used to detect system sleep/wake
	sleepDetectionThreshold time.Duration // Configurable threshold for sleep detection

	// Per-round instance rotation control: ensures we only rotate once per replenish round
	// even when multiple concurrent attempts fail.
	replenishRoundAdvanced int32 // atomic: 1 if already rotated this round, 0 otherwise
}

// NewPool creates a new upstream pool from configuration.
func NewPool(cfg *config.Config) *Pool {
	p := &Pool{
		groups: make(map[string]*Group),
	}

	for name, up := range cfg.Upstreams {
		retryer := retry.DefaultRetryer()
		retryer.MaxAttempts = 0
		log.Printf("[DEBUG] NewPool: upstream=%q ssh.user=%q ssh.key=%q instances=%v",
			name, up.SSH.User, up.SSH.Key, up.Instances)
		p.groups[name] = &Group{
			name: name,
			sshConfig: internalssh.Config{
				User:           up.SSH.User,
				PrivateKeyPath: up.SSH.Key,
			},
			awsCfg:                  up.AWS,
			instances:               up.Instances,
			replenishRetryer:        retryer,
			sleepDetectionThreshold: cfg.SleepDetectionThreshold,
		}
	}

	return p
}

// Connect establishes connections to all upstreams on startup.
// Returns error if any upstream fails to connect (fail fast).
func (p *Pool) Connect(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for name, group := range p.groups {
		log.Printf("[INFO] Connecting to upstream %q...", name)
		if err := group.connect(ctx); err != nil {
			return fmt.Errorf("upstream %q: %w", name, err)
		}
		log.Printf("[INFO] Upstream %q connected via instance %s", name, group.currentInstance())

		// Start connection maintenance goroutine
		group.ctx, group.cancel = context.WithCancel(ctx)
		go group.maintain()
	}

	return nil
}

// Dial connects to the specified address through the named upstream.
func (p *Pool) Dial(ctx context.Context, upstreamName, network, addr string) (net.Conn, error) {
	p.mu.RLock()
	group, ok := p.groups[upstreamName]
	p.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("upstream %q not found", upstreamName)
	}

	return group.dial(ctx, network, addr)
}

const (
	dialTimeoutWindow           = 30 * time.Second
	dialTimeoutsBeforeReconnect = 3
	forcedReconnectCooldown     = 15 * time.Second
)

// selectConn picks a connection from the pool.
//
// We prefer the least-loaded connection (by activeChannels, then inflightDials) to
// reduce head-of-line blocking when one SSH connection is busy with large transfers.
// Returns nil if pool is empty.
func (g *Group) selectConn() *sshConn {
	g.connsMu.RLock()
	defer g.connsMu.RUnlock()

	if len(g.conns) == 0 {
		return nil
	}

	// Find the minimum load across the pool.
	minActive := int64(1 << 62)
	minInflight := int64(1 << 62)
	candidates := make([]*sshConn, 0, len(g.conns))

	for _, sc := range g.conns {
		active := atomic.LoadInt64(&sc.activeChannels)
		inflight := atomic.LoadInt64(&sc.inflightDials)

		if active < minActive || (active == minActive && inflight < minInflight) {
			minActive = active
			minInflight = inflight
			candidates = candidates[:0]
			candidates = append(candidates, sc)
			continue
		}
		if active == minActive && inflight == minInflight {
			candidates = append(candidates, sc)
		}
	}

	if len(candidates) == 0 {
		// Should not happen, but be defensive.
		idx := atomic.AddUint64(&g.nextConn, 1) % uint64(len(g.conns))
		return g.conns[idx]
	}

	// Break ties with round-robin.
	idx := atomic.AddUint64(&g.nextConn, 1) % uint64(len(candidates))
	return candidates[idx]
}

func (g *Group) currentInstance() string {
	g.instanceMu.Lock()
	defer g.instanceMu.Unlock()
	return g.instances[g.current]
}

func (g *Group) advanceInstance() string {
	g.instanceMu.Lock()
	defer g.instanceMu.Unlock()
	g.current = (g.current + 1) % len(g.instances)
	return g.instances[g.current]
}

// selectConnWithCapacity picks a connection from the pool that has capacity for a
// new channel (active + inflight < maxChannelsPerConn). Returns nil if none are
// available.
func (g *Group) selectConnWithCapacity() *sshConn {
	g.connsMu.RLock()
	defer g.connsMu.RUnlock()

	if len(g.conns) == 0 {
		return nil
	}

	minActive := int64(1 << 62)
	minInflight := int64(1 << 62)
	candidates := make([]*sshConn, 0, len(g.conns))

	for _, sc := range g.conns {
		if sc.isDraining() {
			continue
		}
		active := atomic.LoadInt64(&sc.activeChannels)
		inflight := atomic.LoadInt64(&sc.inflightDials)
		if active+inflight >= maxChannelsPerConn {
			continue
		}

		if active < minActive || (active == minActive && inflight < minInflight) {
			minActive = active
			minInflight = inflight
			candidates = candidates[:0]
			candidates = append(candidates, sc)
			continue
		}
		if active == minActive && inflight == minInflight {
			candidates = append(candidates, sc)
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	idx := atomic.AddUint64(&g.nextConn, 1) % uint64(len(candidates))
	return candidates[idx]
}

// startScaleIfNeeded tries to grow the pool when all current connections are at
// capacity. It returns true if it started any new connection attempts.
func (g *Group) startScaleIfNeeded(connID uint64, addr string) bool {
	// Re-check under lock-free reads to avoid doing work if capacity is already
	// available.
	if g.selectConnWithCapacity() != nil {
		return false
	}

	g.scaleMu.Lock()
	defer g.scaleMu.Unlock()

	g.connsMu.RLock()
	poolSize := len(g.conns)
	g.connsMu.RUnlock()

	total := poolSize + g.scaleInFlight
	if total >= maxPoolSize {
		return false
	}
	if g.scaleInFlight >= scaleParallelism {
		return false
	}

	// Add conservatively to avoid stampeding SSM/SSH and triggering remote channel closures.
	remainingPool := maxPoolSize - total
	remainingParallel := scaleParallelism - g.scaleInFlight
	add := 1
	if add > remainingPool {
		add = remainingPool
	}
	if add > remainingParallel {
		add = remainingParallel
	}
	if add <= 0 {
		return false
	}

	g.scaleInFlight += add
	instance := g.currentInstance()

	debugLog("upstream %s: pool saturated, scaling up by %d (pool=%d inflightScale=%d) (trigger conn=%d target=%s)",
		g.name, add, poolSize, g.scaleInFlight, connID, addr)

	for i := 0; i < add; i++ {
		go func() {
			defer func() {
				g.scaleMu.Lock()
				g.scaleInFlight--
				g.scaleMu.Unlock()
			}()

			baseCtx := g.ctx
			if baseCtx == nil {
				baseCtx = context.Background()
			}
			scaleCtx, cancel := context.WithTimeout(baseCtx, 60*time.Second)
			defer cancel()

			sc, err := g.connectSingle(scaleCtx, instance)
			if err != nil {
				log.Printf("[WARN] upstream %s: scale-up connection failed: %v", g.name, err)
				return
			}

			g.connsMu.Lock()
			g.conns = append(g.conns, sc)
			current := len(g.conns)
			g.connsMu.Unlock()

			log.Printf("[INFO] upstream %s: scaled pool connection established (pool=%d) (sshConn=%d)", g.name, current, sc.id)
		}()
	}

	return true
}

// dial attempts to connect through one of the group's SSH connections.
// Uses goroutine + select to respect context timeout since SSH Dial doesn't accept context.
func (g *Group) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	connID, _ := trace.ConnIDFromContext(ctx)
	start := time.Now()

	waitLogged := false

	var sc *sshConn
	for {
		sc = g.selectConnWithCapacity()
		if sc != nil {
			// Reserve capacity for this dial.
			inflight := atomic.AddInt64(&sc.inflightDials, 1)
			if atomic.LoadInt64(&sc.activeChannels)+inflight > maxChannelsPerConn {
				atomic.AddInt64(&sc.inflightDials, -1)
				continue
			}
			break
		}

		g.connsMu.RLock()
		poolSize := len(g.conns)
		g.connsMu.RUnlock()
		if poolSize == 0 {
			return nil, fmt.Errorf("upstream %s: not connected", g.name)
		}

		if !waitLogged {
			waitLogged = true
			debugLog("upstream %s: dial waiting for capacity conn=%d addr=%s (pool=%d maxPool=%d)",
				g.name, connID, addr, poolSize, maxPoolSize)
		}

		_ = g.startScaleIfNeeded(connID, addr)

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("upstream %s: dial wait for capacity: %w", g.name, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer atomic.AddInt64(&sc.inflightDials, -1)

	if sc == nil || sc.sshClient == nil {
		return nil, fmt.Errorf("upstream %s: not connected", g.name)
	}

	g.connsMu.RLock()
	poolSize := len(g.conns)
	g.connsMu.RUnlock()

	debugLog("upstream %s: dial start conn=%d sshConn=%d addr=%s active=%d inflight=%d pool=%d",
		g.name, connID, sc.id, addr, atomic.LoadInt64(&sc.activeChannels), atomic.LoadInt64(&sc.inflightDials), poolSize)

	type dialResult struct {
		conn net.Conn
		err  error
	}
	// Unbuffered channel so we can safely close results when ctx is already done.
	resultCh := make(chan dialResult)

	go func() {
		conn, err := sc.sshClient.Dial(network, addr)
		if err != nil {
			select {
			case resultCh <- dialResult{conn: nil, err: err}:
			case <-ctx.Done():
			}
			return
		}

		// If the caller is no longer waiting (ctx done), close to avoid leaking an SSH channel.
		select {
		case resultCh <- dialResult{conn: conn, err: nil}:
			return
		case <-ctx.Done():
			_ = conn.Close()
			return
		}
	}()

	select {
	case <-ctx.Done():
		debugLog("upstream %s: dial timeout conn=%d sshConn=%d addr=%s dur=%s err=%v",
			g.name, connID, sc.id, addr, time.Since(start), ctx.Err())

		// Track repeated timeouts and evict only the problematic sshConn (instead of
		// reconnecting the whole pool) to reduce blast radius on long-lived transfers.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			count := g.recordDialTimeout(sc)
			if count >= dialTimeoutsBeforeReconnect {
				g.maybeForceReconnect(sc, fmt.Sprintf("%d dial timeouts within %s (last target=%s)", count, dialTimeoutWindow, addr))
			}
		}
		return nil, fmt.Errorf("upstream %s: dial timeout: %w", g.name, ctx.Err())

	case result := <-resultCh:
		if result.err != nil {
			debugLog("upstream %s: dial failed conn=%d sshConn=%d addr=%s dur=%s err=%v",
				g.name, connID, sc.id, addr, time.Since(start), result.err)
			log.Printf("[WARN] upstream %s: dial failed: %v", g.name, result.err)
			// Mark this specific connection as broken; maintain() will handle reconnect.
			g.connsMu.Lock()
			g.removeConn(sc)
			g.connsMu.Unlock()
			return nil, fmt.Errorf("upstream %s: dial failed: %w", g.name, result.err)
		}

		atomic.AddInt64(&sc.activeChannels, 1)
		debugLog("upstream %s: dial ok conn=%d sshConn=%d addr=%s dur=%s active=%d",
			g.name, connID, sc.id, addr, time.Since(start), atomic.LoadInt64(&sc.activeChannels))

		return &trackedConn{Conn: result.conn, sc: sc}, nil
	}
}

// removeConn removes a specific connection from the pool and closes it.
// Caller must hold connsMu write lock.
func (g *Group) removeConn(sc *sshConn) {
	for i, c := range g.conns {
		if c == sc {
			g.conns = append(g.conns[:i], g.conns[i+1:]...)
			if sc.sshClient != nil {
				sc.sshClient.Close()
			}
			if sc.adapter != nil {
				sc.adapter.Close()
			}
			return
		}
	}
}

func (g *Group) recordDialTimeout(sc *sshConn) int64 {
	if sc == nil {
		return 0
	}

	now := time.Now().UnixNano()
	prev := atomic.SwapInt64(&sc.lastDialTimeoutUnixNano, now)

	if prev == 0 || time.Duration(now-prev) > dialTimeoutWindow {
		atomic.StoreInt64(&sc.dialTimeoutCount, 1)
		return 1
	}

	return atomic.AddInt64(&sc.dialTimeoutCount, 1)
}

func (g *Group) maybeForceReconnect(sc *sshConn, reason string) {
	now := time.Now().UnixNano()
	last := atomic.LoadInt64(&g.lastForcedReconnectUnixNano)
	if last != 0 && time.Duration(now-last) < forcedReconnectCooldown {
		return
	}
	if !atomic.CompareAndSwapInt32(&g.forcedReconnectInProgress, 0, 1) {
		return
	}

	atomic.StoreInt64(&g.lastForcedReconnectUnixNano, now)

	if sc == nil {
		log.Printf("[WARN] upstream %s: requested reconnect but no connection was selected (%s)", g.name, reason)
		atomic.StoreInt32(&g.forcedReconnectInProgress, 0)
		return
	}

	log.Printf("[WARN] upstream %s: evicting pool connection (sshConn=%d) (%s)", g.name, sc.id, reason)

	go func() {
		defer atomic.StoreInt32(&g.forcedReconnectInProgress, 0)
		g.connsMu.Lock()
		g.removeConn(sc)
		g.connsMu.Unlock()
	}()
}

// connect establishes the SSH connection pool with failover.
func (g *Group) connect(ctx context.Context) error {
	for attempt := 0; attempt < len(g.instances); attempt++ {
		instance := g.currentInstance()

		if err := g.connectPool(ctx, instance); err != nil {
			log.Printf("[WARN] upstream %s: instance %s failed: %v", g.name, instance, err)
			g.advanceInstance()
			continue
		}

		return nil
	}

	return fmt.Errorf("all instances failed")
}

// connectPool establishes multiple SSH connections to a single instance.
func (g *Group) connectPool(ctx context.Context, instanceID string) error {
	type connResult struct {
		sc  *sshConn
		err error
		idx int
	}

	results := make(chan connResult, defaultPoolSize)
	for i := 0; i < defaultPoolSize; i++ {
		i := i
		go func() {
			connCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			sc, err := g.connectSingle(connCtx, instanceID)
			results <- connResult{sc: sc, err: err, idx: i}
		}()
	}

	var newConns []*sshConn
	var firstErr error
	for i := 0; i < defaultPoolSize; i++ {
		res := <-results
		if res.err != nil {
			if firstErr == nil {
				firstErr = res.err
			}
			log.Printf("[WARN] upstream %s: pool connection %d/%d failed: %v", g.name, res.idx+1, defaultPoolSize, res.err)
			continue
		}
		newConns = append(newConns, res.sc)
		log.Printf("[INFO] upstream %s: pool connection %d/%d established (sshConn=%d)", g.name, res.idx+1, defaultPoolSize, res.sc.id)
	}

	if len(newConns) == 0 {
		return fmt.Errorf("failed to establish any connections: %w", firstErr)
	}

	g.connsMu.Lock()
	g.conns = newConns
	g.connsMu.Unlock()

	log.Printf("[INFO] upstream %s: pool ready with %d connections", g.name, len(newConns))
	return nil
}

// connectSingle establishes a single SSM → SSH connection.
func (g *Group) connectSingle(ctx context.Context, instanceID string) (*sshConn, error) {
	// Create SSM client
	stepStart := time.Now()
	ssmClient, err := ssm.NewClient(ctx, ssm.ClientConfig{
		Profile:   g.awsCfg.Profile,
		Region:    g.awsCfg.Region,
		AccessKey: g.awsCfg.AccessKey,
		SecretKey: g.awsCfg.SecretKey,
	})
	if err != nil {
		return nil, fmt.Errorf("create SSM client: %w", err)
	}
	debugLog("upstream %s: connectSingle step=ssm_client ok dur=%s", g.name, time.Since(stepStart))

	log.Printf("[INFO] upstream %s: starting SSM session to %s (region=%s)...",
		g.name, instanceID, ssmClient.Region())

	// Start SSM session
	stepStart = time.Now()
	session, err := ssmClient.StartSession(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("start session: %w", err)
	}
	debugLog("upstream %s: connectSingle step=start_session ok dur=%s", g.name, time.Since(stepStart))

	// Connect via WebSocket
	stepStart = time.Now()
	adapter, err := protocol.NewAdapter(ctx, session.StreamUrl, session.TokenValue)
	if err != nil {
		return nil, fmt.Errorf("websocket connect: %w", err)
	}
	debugLog("upstream %s: connectSingle step=websocket_connect ok dur=%s", g.name, time.Since(stepStart))

	// Wait for SSM handshake
	stepStart = time.Now()
	if err := adapter.WaitForHandshake(ctx); err != nil {
		adapter.Close()
		return nil, fmt.Errorf("SSM handshake: %w", err)
	}
	debugLog("upstream %s: connectSingle step=ssm_handshake ok dur=%s", g.name, time.Since(stepStart))

	// Establish SSH connection
	stepStart = time.Now()
	sshClient, err := internalssh.Connect(adapter, g.sshConfig)
	if err != nil {
		adapter.Close()
		return nil, fmt.Errorf("SSH connect: %w", err)
	}
	debugLog("upstream %s: connectSingle step=ssh_connect ok dur=%s", g.name, time.Since(stepStart))

	return &sshConn{
		id:        atomic.AddUint64(&nextSSHConnID, 1),
		ssmClient: ssmClient,
		adapter:   adapter,
		sshClient: sshClient,
	}, nil
}

func (g *Group) reserveReplenish(currentCount int) (int, time.Duration) {
	g.replenishMu.Lock()
	defer g.replenishMu.Unlock()

	if g.replenishRetryer == nil {
		g.replenishRetryer = retry.DefaultRetryer()
		g.replenishRetryer.MaxAttempts = 0
	}

	now := time.Now()
	if !g.replenishNextAttempt.IsZero() && now.Before(g.replenishNextAttempt) {
		return 0, time.Until(g.replenishNextAttempt)
	}

	needed := defaultPoolSize - currentCount
	if needed <= 0 {
		return 0, 0
	}

	available := replenishParallelism - g.replenishInFlight
	if available <= 0 {
		return 0, 0
	}

	if needed > available {
		needed = available
	}

	g.replenishInFlight += needed
	return needed, 0
}

func (g *Group) finishReplenish() {
	g.replenishMu.Lock()
	if g.replenishInFlight > 0 {
		g.replenishInFlight--
	}
	g.replenishMu.Unlock()
}

func (g *Group) recordReplenishSuccess() {
	g.replenishMu.Lock()
	g.replenishFailures = 0
	g.replenishNextAttempt = time.Time{}
	g.replenishMu.Unlock()
}

func (g *Group) recordReplenishFailure() (time.Duration, int) {
	g.replenishMu.Lock()
	defer g.replenishMu.Unlock()

	if g.replenishRetryer == nil {
		g.replenishRetryer = retry.DefaultRetryer()
		g.replenishRetryer.MaxAttempts = 0
	}

	g.replenishFailures++
	delay := g.replenishRetryer.NextDelay(g.replenishFailures)
	g.replenishNextAttempt = time.Now().Add(delay)
	return delay, g.replenishFailures
}

// resetReplenishBackoff clears the backoff state to allow immediate retry.
// Used after detecting system sleep/wake to recover quickly.
func (g *Group) resetReplenishBackoff() {
	g.replenishMu.Lock()
	defer g.replenishMu.Unlock()

	g.replenishFailures = 0
	g.replenishNextAttempt = time.Time{}
	g.replenishInFlight = 0
}

func (g *Group) replenishOnce(instance string, attemptID uint64) {
	defer g.finishReplenish()

	baseCtx := g.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}

	ctx, cancel := context.WithTimeout(baseCtx, replenishTimeout)
	defer cancel()

	start := time.Now()
	debugLog("upstream %s: replenish attempt start (attempt=%d instance=%s timeout=%s)", g.name, attemptID, instance, replenishTimeout)

	sc, err := g.connectSingle(ctx, instance)
	if err != nil {
		delay, failures := g.recordReplenishFailure()
		log.Printf("[WARN] upstream %s: replenish attempt failed (attempt=%d) after %s: %v (backoff=%s failures=%d)", g.name, attemptID, time.Since(start), err, delay, failures)

		// Only rotate instance once per replenish round to avoid pointer wraparound
		// when multiple concurrent attempts fail (e.g., 2 failures with 2 instances
		// would rotate back to the original instance).
		if len(g.instances) > 1 {
			if atomic.CompareAndSwapInt32(&g.replenishRoundAdvanced, 0, 1) {
				next := g.advanceInstance()
				debugLog("upstream %s: rotated to next instance %s after failure", g.name, next)
			}
		}
		return
	}

	g.connsMu.Lock()
	g.conns = append(g.conns, sc)
	current := len(g.conns)
	g.connsMu.Unlock()

	g.recordReplenishSuccess()
	log.Printf("[INFO] upstream %s: replenished pool connection (%d/%d) (sshConn=%d) (attempt=%d)", g.name, current, defaultPoolSize, sc.id, attemptID)
}

// maintain monitors connections and replenishes the pool on failure.
func (g *Group) maintain() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	g.lastMaintainTime = time.Now()

	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
		}

		now := time.Now()

		// Detect system sleep/wake: if tick interval is much larger than expected,
		// the system was likely asleep. Reset backoff and clear stale connections.
		if !g.lastMaintainTime.IsZero() && g.sleepDetectionThreshold > 0 {
			elapsed := now.Sub(g.lastMaintainTime)
			if elapsed > g.sleepDetectionThreshold {
				log.Printf("[INFO] upstream %s: detected system wake after %s, resetting backoff and clearing stale connections",
					g.name, elapsed)
				g.resetReplenishBackoff()
				// Mark all connections as draining and move them out of the pool.
				// They will be closed gracefully once idle.
				g.connsMu.Lock()
				oldConns := g.conns
				for _, sc := range oldConns {
					sc.markDraining()
				}
				g.conns = nil // Trigger replenish with fresh connections
				g.connsMu.Unlock()

				// Gracefully close old connections in background once they become idle.
				go g.closeDrainingConns(oldConns, 30*time.Second)
			}
		}
		g.lastMaintainTime = now

		// Check pool health: remove dead connections and replenish.
		g.connsMu.Lock()
		var alive []*sshConn
		for _, sc := range g.conns {
			if sc.adapter != nil {
				select {
				case <-sc.adapter.Done():
					// Connection dead, close and skip.
					log.Printf("[INFO] upstream %s: pool connection closed, removing (sshConn=%d)", g.name, sc.id)
					if sc.sshClient != nil {
						sc.sshClient.Close()
					}
					sc.adapter.Close()
				default:
					alive = append(alive, sc)
				}
			}
		}
		g.conns = alive
		currentCount := len(alive)
		g.connsMu.Unlock()

		// Replenish if pool is below target size.
		if currentCount < defaultPoolSize {
			needed, wait := g.reserveReplenish(currentCount)
			if wait > 0 {
				debugLog("upstream %s: replenish backoff active, next attempt in %s", g.name, wait)
				continue
			}
			if needed == 0 {
				continue
			}

			log.Printf("[INFO] upstream %s: pool has %d/%d connections, replenishing %d",
				g.name, currentCount, defaultPoolSize, needed)

			// Reset the per-round rotation flag: only one failed attempt in this round
			// should trigger instance rotation to avoid pointer wraparound.
			atomic.StoreInt32(&g.replenishRoundAdvanced, 0)

			// Capture instance once per round so all concurrent attempts use the same target.
			instance := g.currentInstance()

			for i := 0; i < needed; i++ {
				attemptID := atomic.AddUint64(&g.replenishAttemptID, 1)
				go g.replenishOnce(instance, attemptID)
			}
		}
	}
}

// cleanup closes all connections in the pool.
func (g *Group) cleanup() {
	for _, sc := range g.conns {
		if sc.sshClient != nil {
			sc.sshClient.Close()
		}
		if sc.adapter != nil {
			sc.adapter.Close()
		}
	}
	g.conns = nil
}

// Close closes all upstream connections.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, g := range p.groups {
		if g.cancel != nil {
			g.cancel()
		}
		g.connsMu.Lock()
		g.cleanup()
		g.connsMu.Unlock()
	}
}
