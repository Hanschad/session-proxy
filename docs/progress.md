# Progress Report - Jan 3, 2026

## Status: ✅ WORKING

The session-proxy is now fully functional. SSM handshake, SSH tunnel, and SOCKS5 proxy all work correctly.

## Achievements
1.  **Fixed WebSocket Connection**: Resolved an issue where the WebSocket URL was malformed due to incorrect query parameter appending.
2.  **Implemented Binary Protocol**: Replaced the incorrect JSON-based message protocol with the correct AWS SSM binary protocol in `internal/protocol/message.go`.
3.  **Handled SSM Handshake**: Implemented filtering in `internal/protocol/adapter.go` to interpret and ignore the initial `RequestedClientActions` JSON message from the SSM Agent, preventing it from corrupting the SSH handshake.
4.  **Reverse Engineered ACK Format**: Analyzed server traffic to determine the correct payload format for Acknowledge messages (4-byte length prefix + specific JSON structure).

## Jan 3 Fixes (Based on SESSION_MANAGER_CONNECTION_ANALYSIS.md)

5.  **Fixed ACK Message Format**:
    - Removed incorrect 4-byte length prefix from ACK payload - ACK payload is pure JSON per session-manager-plugin source
    - Fixed Flags value: ACK messages use `FlagData=0`, not `AckFlag=3` (Flags is a bitmask for SYN/FIN stream control, not message type)
    - ACK sequence number set to 0 as ACKs don't participate in message sequencing

6.  **Improved OpenDataChannel Handshake**:
    - Added `ClientId` and `ClientVersion` fields per section 5.4 of the analysis
    - Token is now sent only in the OpenDataChannel JSON message, not appended to URL

7.  **Enhanced Handshake Protocol Handling**:
    - Added PayloadType-based filtering (PayloadType 5=HandshakeRequest, 6=Response, 7=Complete)
    - Implemented `handleHandshakeRequest()` to send proper HandshakeResponse
    - Added handling for `channel_closed` message type
    - Improved legacy fallback detection for older agent versions

8.  **Added PayloadType Constants**: Defined all PayloadType values from the analysis doc for proper message handling.

9.  **Fixed Sequence Number Off-by-One Error** (CRITICAL):
    - **Root Cause**: `nextSeq()` was incrementing BEFORE returning, causing first message to have seq=1 instead of seq=0
    - **Impact**: SSM Agent silently rejected HandshakeResponse (wrong seq), kept retransmitting HandshakeRequest
    - **Fix**: Changed to return current value THEN increment: `seq := a.seqNum; a.seqNum++; return seq`
    - **Reference**: See `docs/troubleshooting-ssm-handshake.md` for full analysis

10. **Added Message Deduplication**:
    - Implemented `seenMsgIds` map to track processed message IDs
    - Added `handshakeResponded` flag to prevent duplicate HandshakeResponse
    - Agent retransmissions are now properly ACKed without re-processing

11. **Added Handshake Synchronization**:
    - Implemented `handshakeDone` channel and `WaitForHandshake(ctx)` method
    - SSH connection now waits for SSM handshake completion before starting

## Verified Working
- ✅ SSM session establishment
- ✅ SSM handshake (HandshakeRequest → HandshakeResponse → HandshakeComplete)
- ✅ SSH handshake over SSM tunnel
- ✅ SOCKS5 proxy listening and accepting connections

## Known Behaviors
- SSM Agent continues retransmitting HandshakeRequest even after handshake completes (normal behavior, handled by deduplication)

## Jan 6 Fixes (Based on DUPLICATE_HANDSHAKE_FIX.md and MESSAGE_REORDER_ISSUE.md)

12. **Fixed Persistent HandshakeRequest Retransmission**:
    - **Problem**: Agent kept resending HandshakeRequest despite ACKs.
    - **Root Causes Identified**:
      1. **MessageType Padding**: Proxy used null-bytes (0x00) for 32-byte padding, AWS uses spaces (0x20).
      2. **UUID Byte Order**: AWS Agent uses a custom byte order (LSB first for bytes 8-16, then MSB for bytes 0-8) for `MessageId` serialization. Proxy used standard BigEndian.
    - **Fix**: Adjusted `MarshalBinary` and `UnmarshalMessage` to match AWS specific binary format exactly.
    - **Result**: Handshake completes cleanly without infinite retransmissions.
    - **Documentation**: [DUPLICATE_HANDSHAKE_FIX.md](DUPLICATE_HANDSHAKE_FIX.md)

13. **Fixed SSH "Max Packet Length Exceeded" Error**:
    - **Problem**: SSH connection would drop with "max packet length exceeded" after some data transfer.
    - **Cause**: Out-of-order message delivery (e.g., Seq 25 arriving after Seq 36). The proxy was forwarding raw payloads directly to the SSH client without reordering, corrupting the SSH stream.
    - **Fix**: Implemented `incomingMsgBuffer` and `expectedSeqNum` logic in `Adapter`. Messages are now buffered and processed strictly in sequence order.
    - **Result**: Stable SSH connection and data transfer.
    - **Documentation**: [MESSAGE_REORDER_ISSUE.md](MESSAGE_REORDER_ISSUE.md)

## Jan 8 Features: Multi-Upstream Routing

14. **Configuration System** (`internal/config`):
    - Viper-based config loading (YAML/TOML).
    - Search priority: CLI flags > `--config` > `./config.yaml` > XDG path.
    - SSH credentials now **per upstream** (not global).
    - AWS profile vs inline credentials validation.

15. **Route-Based Upstream Selection** (`internal/router`):
    - CIDR matching: `10.0.0.0/8` → upstream `dev`.
    - Domain glob matching: `*.prod.internal` → upstream `prod`.
    - Default upstream fallback.

16. **Upstream Pool with Failover** (`internal/upstream`):
    - Multiple SSH connections managed per upstream group.
    - **Startup connection**: `Connect()` establishes all connections, fails fast.
    - **Automatic reconnection**: `maintain()` goroutine monitors and reconnects.
    - **Passive failover**: On dial failure, tries next instance in list.

17. **AWS Region Auto-Detection**:
    - When using profile: region auto-detected from AWS config.
    - When using inline credentials: explicit region required.

18. **SOCKS5 Authentication** (`internal/socks5`):
    - RFC 1929 username/password authentication.
    - Optional, configured per listen port.

19. **Backward Compatibility**:
    - Legacy single-target mode still works: `./session-proxy --target i-xxx`.
    - New config mode: `./session-proxy` or `./session-proxy --config path/to/config.yaml`.

## Current Status

- ✅ Multi-upstream routing with CIDR/domain matching
- ✅ Automatic failover within upstream group
- ✅ SOCKS5 authentication (optional)
- ✅ 13 tests passing (config: 5, router: 3, socks5: 5)
- ✅ Backward compatible with legacy CLI mode

## Roadmap (Backlog)

- [ ] CLI flags for all config options
- [ ] Direct connection when no routes match
- [ ] TUN transparent proxy (future project)

