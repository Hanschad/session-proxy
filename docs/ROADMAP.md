# Roadmap

## Phase 1: Observability

### 1.1 Structured Logging

**Problem**: Current `[DEBUG]` logs are interleaved and hard to filter in high concurrency.

**Solution**: Migrate to `slog` with structured fields.

```go
slog.Debug("dial upstream",
    "upstream", upstream.Name,
    "target", addr,
    "seq", sequenceNum,
)
```

**Key fields**: `upstream`, `target`, `seq`, `request_id`, `duration_ms`

---

### 1.2 Prometheus Metrics

**Problem**: No runtime observability for production monitoring.

**Solution**: Expose `/metrics` endpoint.

**Metrics**:
- `socks5_connections_total{upstream}` - Counter
- `socks5_active_connections{upstream}` - Gauge
- `upstream_dial_duration_seconds{upstream}` - Histogram
- `upstream_reconnect_total{upstream}` - Counter
- `upstream_connection_status{upstream}` - Gauge (1=connected, 0=disconnected)

---

## Phase 2: Connection Management

### 2.1 Connection Pooling

**Problem**: Single SSH connection per upstream; potential bottleneck under extreme load.

**Solution**: Maintain N connections per upstream with load balancing.

**Config**:
```yaml
upstreams:
  production:
    address: bastion.example.com
    pool_size: 3           # default: 1
    strategy: round-robin  # or: least-connections
```

**Note**: SSH channel multiplexing handles most cases. This is for scenarios where a single TCP connection becomes a bandwidth bottleneck.

---

## Phase 3: Proxy Chaining

### 3.1 Multi-Protocol Upstream

**Problem**: Some networks require routing through HTTP CONNECT or external SOCKS5 proxies.

**Solution**: Extend `Upstream` to support multiple proxy types.

**Config**:
```yaml
upstreams:
  corp-proxy:
    type: http         # http | socks5 | ssh (default)
    address: proxy.corp:8080
    
  external-socks:
    type: socks5
    address: socks.vpn.com:1080
    auth:
      username: user
      password: pass
```

**Implementation**: Abstract `Dialer` interface per upstream type.

---

## Phase 4: Transparent Proxy

### 4.1 TUN Device Integration

**Problem**: Applications require manual SOCKS5 configuration.

**Goal**: Auto-intercept traffic like Tailscale — start daemon, traffic flows through proxy automatically.

**Approach**:
1. Create TUN device (`utun` on macOS)
2. Configure routing table for target CIDRs
3. Userspace TCP/IP stack to handle intercepted packets
4. Route connections through upstream SOCKS5/SSH

**Dependencies**: `gvisor/netstack` or `wireguard-go/tun`

**Complexity**: High — requires userspace networking and privilege escalation.

---

## Priority

| Phase | Items | Effort | Value |
|-------|-------|--------|-------|
| 1 | Logging + Metrics | Low | High |
| 2 | Connection Pool | Medium | Medium |
| 3 | Proxy Chaining | Medium | High |
| 4 | Transparent Proxy | High | High |
