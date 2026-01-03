# SSM Handshake Troubleshooting Guide

## Problem Summary

SSM Agent kept retransmitting `HandshakeRequest` messages even after we sent `HandshakeResponse` and received ACK for it. The handshake never completed.

## Root Cause

**Sequence Number Off-by-One Error**

The SSM Agent expects the first message from the plugin to have `SequenceNumber = 0`. However, our `nextSeq()` function was incrementing BEFORE returning:

```go
// WRONG - first message gets seq=1
func (a *Adapter) nextSeq() int64 {
    a.seqNum++
    return a.seqNum
}
```

This caused our `HandshakeResponse` to be sent with `Seq=1`, but the Agent's `ExpectedSequenceNumber` was `0`. The Agent silently rejected the message and kept retransmitting `HandshakeRequest`.

## The Fix

```go
// CORRECT - first message gets seq=0
func (a *Adapter) nextSeq() int64 {
    seq := a.seqNum
    a.seqNum++
    return seq
}
```

## How We Found It

### 1. Symptom Analysis

Debug logs showed:
- `HandshakeRequest` (PayloadType=5, Seq=0) received ✓
- ACK sent for HandshakeRequest ✓
- `HandshakeResponse` (PayloadType=6, Seq=1) sent ✓
- ACK received for our message (Seq=1 ACK) ✓
- But Agent kept retransmitting HandshakeRequest

The ACK for seq=1 came back, meaning our message was received at the transport layer, but the Agent didn't process it as a valid HandshakeResponse.

### 2. Source Code Analysis

Examined the official AWS implementations:

**Plugin side** (`session-manager-plugin/src/datachannel/streaming.go:306-327`):
```go
func (dataChannel *DataChannel) SendMessage(log log.T, input []byte, inputType int) error {
    // Uses sequence number BEFORE incrementing
    msg.SequenceNumber = dataChannel.StreamDataSequenceNumber
    // ...
    dataChannel.StreamDataSequenceNumber = dataChannel.StreamDataSequenceNumber + 1
}
```

**Agent side** (`amazon-ssm-agent/agent/session/datachannel/datachannel.go:243`):
```go
func (dataChannel *DataChannel) Initialize(...) {
    // Agent starts expecting sequence 0
    dataChannel.ExpectedSequenceNumber = 0
}
```

**Agent validation** (`datachannel.go:588`):
```go
func (dataChannel *DataChannel) handleStreamDataMessage(...) {
    // Only processes messages matching expected sequence
    if streamDataMessage.SequenceNumber == dataChannel.ExpectedSequenceNumber {
        // Process message
        dataChannel.ExpectedSequenceNumber++
    }
}
```

### 3. Conclusion

The Agent uses `ExpectedSequenceNumber` starting at 0 to validate incoming messages. Any message with a different sequence number is silently dropped (only ACKed at transport layer). Our first message had seq=1, so it was never processed.

## Key Debugging Techniques

1. **Add binary-level debug logging** - Log the raw bytes being sent to verify header fields
2. **Compare with official implementation** - Read the AWS plugin/agent source code, don't guess the protocol
3. **Understand the difference between transport ACK and application processing** - A message can be ACKed but still rejected at the application layer

## Related Files

- `internal/protocol/adapter.go` - Adapter implementation with `nextSeq()`
- `internal/protocol/message.go` - Binary message serialization
- AWS Plugin: `session-manager-plugin/src/datachannel/streaming.go`
- AWS Agent: `amazon-ssm-agent/agent/session/datachannel/datachannel.go`

## SSM Handshake Protocol Reference

```
1. Agent -> Plugin: HandshakeRequest (PayloadType=5, Seq=0)
   - Contains AgentVersion, RequestedClientActions

2. Plugin -> Agent: ACK for HandshakeRequest

3. Plugin -> Agent: HandshakeResponse (PayloadType=6, Seq=0)
   - Contains ClientVersion, ProcessedClientActions
   - CRITICAL: Seq must be 0

4. Agent -> Plugin: ACK for HandshakeResponse

5. Agent -> Plugin: HandshakeComplete (PayloadType=7, Seq=1)
   - Contains HandshakeTimeToComplete

6. Plugin -> Agent: ACK for HandshakeComplete

7. Session ready for data transfer
```

## Lessons Learned

1. **Sequence numbers in protocols are 0-indexed** - First message should have seq=0
2. **Silent failures are hard to debug** - When a message is ACKed but not processed, there's no error feedback
3. **Always refer to official source code** - Protocol documentation may be incomplete; the source is the truth
