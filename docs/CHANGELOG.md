# Changelog

All notable changes to this project will be documented in this file.

## v0.0.4 (Jan 15, 2026)

### Reliability / Large Transfers
- Implemented MGS flow-control handling (`pause_publication` / `start_publication`)
- Added outgoing ACK tracking + retransmission loop (oldest-only resend)
- Added backpressure for TCP->SSM bridging with `SESSION_PROXY_SSM_MAX_UNACKED_BYTES`
- Default tuning aligned for stability: chunk size 1024, conservative default unacked window 256KB

### Observability
- Reduced debug log spam for outgoing backpressure (log-once per wait cycle)

### Tests & Docs
- Added unit tests for ACK/outgoing removal and mismatch handling
- Added troubleshooting doc: `large-transfer-stability.md`

## v0.0.3 (Jan 9, 2026)

### Code Quality
- Router `Match()` now thread-safe with `RLock()`
- Improved message deduplication (sequence-based eviction)
- Added tests for `proxy` and `ssh` modules

### Documentation
- Added `README.md`, `LICENSE`, `Makefile`
- Standardized doc naming (lowercase-kebab-case)
- Cleaned up redundant troubleshooting docs

### Housekeeping
- Updated `.gitignore` for Go projects
- Removed `AGENT.md` from version control

## v0.0.2 (Jan 8, 2026)

### Features
- **Multi-upstream routing**: CIDR and domain glob matching
- **Upstream pool with failover**: Auto-reconnection, passive failover
- **SOCKS5 authentication**: RFC 1929 username/password (optional)
- **Hot-reload**: Routes updated without restart
- **Direct connection**: When no routes match

### Configuration
- Viper-based YAML/TOML config
- Per-upstream SSH/AWS credentials
- AWS profile auto-detection
- CLI flags for all options

## v0.0.1 (Jan 6, 2026)

### Features
- SSM WebSocket connection
- SSH tunnel over SSM
- SOCKS5 proxy (single upstream)

### Bug Fixes
- Fixed MessageType 32-byte padding (null → space)
- Fixed UUID byte order (AWS swapped LSB/MSB)
- Implemented message reordering buffer
- Fixed sequence number off-by-one

### Documentation
- [handshake-fix.md](handshake-fix.md)
- [message-reorder-fix.md](message-reorder-fix.md)
