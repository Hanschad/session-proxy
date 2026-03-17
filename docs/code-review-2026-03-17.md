# Deep Code Review Report (2026-03-17)

## Scope

Review target: current `main` branch codebase.

Primary areas inspected:

- `cmd/session-proxy`
- `internal/config`
- `internal/proxy`
- `internal/router`
- `internal/socks5`
- `internal/protocol`
- `internal/ssh`
- `internal/upstream`

## Review Method

The review focused on:

- request routing correctness
- connection lifecycle and failure recovery
- protocol boundary handling
- authentication and security-sensitive behavior
- concurrency and resource management
- test coverage gaps around fault handling

Checks run during review:

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`

At review time, all three checks completed without diagnostics. That means the current issues are mostly semantic and operational, not simple compile-time or race-detector failures.

## Findings

### P1: Upstream dial failures evict healthy pooled SSH transports

Affected file: `internal/upstream/pool.go`

Relevant logic:

- `Group.dial()`
- `removeConn()`

Current behavior:

When `sshClient.Dial()` returns any error, the code removes the selected pooled SSH connection from the pool and closes it.

Why this is a problem:

- Many `Dial()` failures are target-specific, not transport-specific.
- Examples include connection refused, host unreachable, or remote-side open failure for a single destination.
- In those cases, tearing down the whole SSH transport is too aggressive.
- Because one SSH connection carries multiple channels, evicting it can also terminate unrelated in-flight traffic sharing that transport.

Impact:

- unnecessary reconnect churn
- avoidable disruption to healthy streams
- higher latency under intermittent destination failures

Recommendation:

- distinguish per-target connect failures from transport corruption
- only evict the pooled connection when there is evidence the SSH transport itself is broken
- add tests for refused/unreachable target errors versus actual transport teardown

### P1: Handshake wait path does not fail fast when adapter closes early

Affected file: `internal/protocol/adapter.go`

Relevant logic:

- `WaitForHandshake()`
- `readLoop()`

Current behavior:

`WaitForHandshake()` waits only for handshake completion or context cancellation. If the WebSocket or SSM stream dies before handshake completion, `readLoop()` closes the adapter, but that does not wake `WaitForHandshake()` immediately.

Why this is a problem:

- early handshake failures degrade into full context timeout waits
- current connection establishment paths use long timeouts, so startup and failover can stall much longer than necessary
- repeated reconnect attempts become significantly slower under partial outages

Impact:

- slow startup on broken upstreams
- slow failover and replenishment
- poor operator feedback during real incidents

Recommendation:

- make `WaitForHandshake()` also observe adapter shutdown
- return the underlying close/read error when possible
- add a test for "adapter closes before handshake complete"

### P1: Weak-network backpressure can stall forever instead of converging to failure

Affected file: `internal/protocol/adapter.go`

Relevant logic:

- `addOutgoing()`
- `resendLoop()`
- `waitForPublication()`

Current behavior:

Under poor network conditions, the adapter can block writers on unacknowledged-byte limits or `pause_publication`, while the resend loop keeps retrying the oldest frame indefinitely.

Why this is a problem:

- there is no resend budget or stream-level timeout that turns a prolonged ACK stall into a bounded failure
- a lossy or high-latency path can therefore degrade into very long application-visible timeouts
- the tunnel appears "connected but stuck", which is worse operationally than failing fast and rebuilding

Impact:

- sustained trading or request streams can hang instead of reconnecting
- weak-network behavior feels significantly worse than `aws cli + ssh`
- backpressure can accumulate into end-user request timeouts even when the process itself is still alive

Recommendation:

- introduce resend budgets or a stream timeout for prolonged ACK stalls
- surface a clear transport failure when flow control does not recover within a bounded period
- add tests that simulate missing ACKs, prolonged `pause_publication`, and lossy links

### P2: SOCKS relay drops half-close semantics for SSH-backed connections

Affected file: `internal/socks5/server.go`

Relevant logic:

- `relay()`

Current behavior:

The relay only calls `CloseWrite()` when the connection is a `*net.TCPConn`. SSH direct-tcpip channels also support write-side close, but that path is not used.

Why this is a problem:

- some protocols rely on EOF to indicate request completion
- without half-close propagation, the upstream side may keep waiting for more request bytes
- the proxy can therefore turn a correct request/response interaction into an idle wait and eventual timeout

Impact:

- request/response protocols tunneled over SSH can appear to hang
- long-lived transaction flows may intermittently stall even when the underlying path is otherwise healthy

Recommendation:

- propagate half-close through any `CloseWrite()`-capable connection, not only `*net.TCPConn`
- add regression coverage for SSH-backed relay semantics

### P2: addOutgoing backpressure wait has a deadlock window on adapter close

Affected file: `internal/protocol/adapter.go`

Relevant logic:

- `addOutgoing()`

Current behavior:

When the outgoing buffer is full, `addOutgoing()` calls `outgoingCond.Wait()` (line 467), then checks `a.done` via a non-blocking select (lines 468-472). This check is not atomic with respect to the `Wait()` return: if the adapter closes between `Wait()` returning and the `select`, the `default` branch may execute first, causing the goroutine to loop back into `Wait()`. At that point, no further `Broadcast()` will arrive because `Close()` already broadcast and `resendLoop` has exited.

Why this is a problem:

- writer goroutines can hang forever on a closed adapter
- the symptom is a "connected but stuck" tunnel that never recovers
- this is particularly likely under sustained write load when the adapter dies mid-transfer

Impact:

- transaction streams appear to hang instead of failing with an error
- the proxy looks alive but stops forwarding data

Recommendation:

- restructure the wait loop to check `a.done` before and after `Wait()` in a race-free manner, e.g. `for { select { case <-a.done: return err; default: } ; if buffer_ok { break } ; cond.Wait() }`
- add a test that closes the adapter while a writer is blocked on backpressure

### P2: pongWait too long (2min10s) delays detection of half-dead connections

Affected file: `internal/protocol/adapter.go`

Relevant logic:

- `pongWait` constant (line 147)
- `PingInterval` constant (line 142)

Current behavior:

`pongWait` is set to `2*PingInterval + 10*time.Second` = 2 minutes 10 seconds. A connection that stops responding to pings will not be detected as dead until this timeout expires.

Why this is a problem:

- native `aws cli + ssh` typically detects dead connections within 15-30 seconds via SSH keepalive
- a half-dead connection sits in the pool for over 2 minutes, accepting new channel assignments but unable to deliver data
- during this window, any traffic routed to this connection will hang or timeout at the application level

Impact:

- weak-network experience is significantly worse than native SSH
- pool appears to have capacity but selected connections are unresponsive
- user-visible latency spikes of up to 2 minutes before the pool self-heals

Recommendation:

- reduce `PingInterval` to ~15-20 seconds and `pongWait` to ~35-45 seconds
- align dead-connection detection speed with what users expect from native SSH behavior
- consider adding an SSH-level keepalive (`SendRequest("keepalive@openssh.com", ...)`) as a secondary health check

### P2: Successful ssh-agent authentication leaks file descriptors

Affected file: `internal/ssh/client.go`

Relevant logic:

- agent socket setup in `Connect()`

Current behavior:

When agent authentication is available and used, the unix socket connected through `SSH_AUTH_SOCK` remains open. It is only closed on the branch where the agent has no usable keys or returns an error.

Why this is a problem:

- connection pool startup creates multiple SSH sessions
- replenish and scale-up paths create additional sessions over time
- each successful agent-based connection can leak one file descriptor

Impact:

- gradual file descriptor exhaustion in long-running processes
- hard-to-diagnose failures after extended runtime

Recommendation:

- explicitly close the agent connection after SSH handshake finishes
- document the intended lifetime of the agent socket in code
- add a regression test around agent socket lifecycle if practical

### P3: Config validation blocks explicit `DIRECT` routes

Affected files:

- `internal/config/config.go`
- `internal/router/router.go`
- `internal/proxy/server.go`

Current behavior:

Runtime routing supports a special upstream value `DIRECT`, but config validation rejects any route target that is not present in `upstreams`.

Why this is a problem:

- the code advertises explicit direct-routing support
- users cannot actually configure that path through the validated config format
- documentation and runtime behavior are inconsistent

Impact:

- feature is effectively unavailable
- configuration intent cannot be expressed cleanly

Recommendation:

- allow `DIRECT` as a special-case route target during validation
- document the precedence and expected behavior clearly
- add config and router tests for explicit `DIRECT` rules

## Operational Analysis

The user-reported symptoms:

- sustained transaction streams eventually disconnect or time out
- weak-network experience is worse than `aws cli + ssh`

are consistent with the current architecture and implementation details.

### Why sustained streams disconnect or time out

- This project reimplements the SSM data-channel behavior in userspace instead of delegating to the AWS-maintained plugin path.
- Multiple business TCP streams are multiplexed onto a smaller SSH connection pool, so the blast radius of a single transport decision is larger.
- A target-specific `sshClient.Dial()` error can currently evict an entire pooled SSH transport, interrupting unrelated streams.
- When ACKs stop advancing under weak networks, the adapter currently prefers indefinite backpressure/retry over bounded failure and rebuild.
- Active tunneled TCP flows do not have transparent continuity once the underlying adapter dies; recovery only helps future connections.

### Why weak-network behavior is worse than `aws cli + ssh`

- `aws cli + ssh` relies on the AWS-maintained Session Manager path, which has a more mature recovery model and more battle-tested transport behavior.
- This proxy deliberately uses conservative defaults (`1024`-byte SSM chunks and a `256KB` unacknowledged window) to avoid `channel_closed`, which improves safety but reduces tolerance for high RTT and delayed ACKs.
- Once the proxy enters `pause_publication` or unacked-byte backpressure, it can remain stuck for too long before surfacing a hard failure.
- Handshake failures also recover more slowly than they should because the current wait path depends on outer context timeout.

In short, the current implementation is optimized for protocol safety and basic stability, but it still lacks some of the convergence behavior needed for poor networks and continuous low-latency transactional traffic.

## Review of Current Fixes

This review pass also inspected the current uncommitted fixes in:

- `internal/config/config.go`
- `internal/protocol/adapter.go`
- `internal/socks5/server.go`
- `internal/ssh/client.go`
- `internal/upstream/pool.go`

### Changes that look correct

- Explicit `DIRECT` route targets and `default: DIRECT` are now accepted by config validation, which completes the advertised direct-routing mode.
- `WaitForHandshake()` now observes adapter shutdown and no longer waits only for the outer context timeout.
- `WaitForHandshake()` now prefers a completed handshake over a concurrent shutdown signal and can preserve the underlying close reason.
- Handshake control-frame write failures now close the adapter immediately instead of falling back to ping/context timeout.
- SOCKS relay now propagates `CloseWrite()` through any compatible connection type, which restores half-close semantics for SSH-backed relays.
- ssh-agent sockets are now closed after the SSH handshake path completes, which fixes the file descriptor leak.
- Upstream dial failures are now classified before evicting a pooled SSH transport, which is the right direction for reducing blast radius.
- The pooled-transport classifier now recognizes canonical `net.ErrClosed` failures instead of relying only on legacy error strings.
- `pause_publication` now has a bounded close path instead of relying on unbounded waiting.
- The current patch also adds focused tests for `DIRECT` validation, direct-route/default matching, `CloseWrite()` propagation, `isTransportError()` classification, handshake close-reason propagation, handshake write-failure shutdown, and real adapter close behavior for resend/pause timeouts.

### New or remaining concerns in the current patch

#### P3: Ping timing has improved but still lacks validation coverage

Affected file: `internal/protocol/adapter.go`

Current behavior:

`PingInterval` was reduced from 1 minute to 30 seconds and `pongWait` now gives roughly 70 seconds of silence tolerance.

Why this matters:

- this is directionally better than the previous 2m10s detection window
- however, there is still no regression coverage showing how the new values behave under packet loss or transient congestion

Recommendation:

- add tests or controlled packet-loss validation before treating the new values as settled defaults

## Coverage Gaps

The current tests are generally healthy for happy paths, but several failure paths are still thin or only partially covered:

- end-to-end classification of `sshClient.Dial()` errors
- prolonged ACK stall / resend convergence beyond the current attempt/time limits
- half-close propagation across real SSH-backed relay connections
- ssh-agent resource cleanup over repeated reconnect cycles
- behavior of the new ping/pong timing under packet loss

These are exactly the areas where the review found the highest-value issues.

## Suggested Fix Order

1. Validate and tune weak-network convergence in `internal/protocol`, especially around ACK stalls and retransmission policy.
2. Strengthen `sshClient.Dial()` transport-error classification and add behavior-level tests in `internal/upstream`.
3. Validate ping/pong timing under degraded networks before treating the new values as settled defaults.
4. Add longer-running coverage for relay half-close and ssh-agent cleanup paths.

## Summary

The codebase is in decent shape mechanically: tests pass, race detection is clean, and the main architecture is coherent. The main risks are in failure semantics rather than baseline functionality.

The highest-priority remaining issues are operational:

- weak-network flow control still needs stronger convergence validation under loss and high RTT
- pooled transport error classification still depends on heuristic matching of SSH error strings
- ping/pong defaults have improved, but their degraded-network behavior is not yet validated

Several previously identified issues are now addressed correctly in the current working tree, including `DIRECT` validation, handshake fast-fail, close-reason propagation for early adapter shutdown, relay half-close propagation, and ssh-agent cleanup. The remaining risk is concentrated in the weak-network handling policy and its test coverage rather than in obvious correctness bugs.
