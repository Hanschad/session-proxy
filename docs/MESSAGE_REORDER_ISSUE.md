# 消息乱序问题排查与修复

本文档记录了 SSH 数据传输阶段 `ssh: max packet length exceeded` 错误的完整排查和修复过程。

## 问题现象

修复 HandshakeRequest 重复问题后，SSH 连接建立成功，但数据传输阶段出现错误：

```
2026/01/06 13:39:28 ssh: max packet length exceeded
```

连接在传输几十个数据包后突然中断。

---

## 排查过程

### 第 1 步：分析日志找到异常点

```
2026/01/06 13:39:28 RX Frame: ... Seq=36 ...
2026/01/06 13:39:28 TX SSH Data: 788 bytes
2026/01/06 13:39:28 RX Frame: ... Seq=25 ...  ← Seq=25 在 Seq=36 之后到达！
2026/01/06 13:39:28 Closing Adapter
2026/01/06 13:39:28 ssh: max packet length exceeded
```

**关键发现**：消息乱序到达 (Seq=25 在 Seq=36 之后)。

`ssh: max packet length exceeded` 是 Go 的 `crypto/ssh` 包在尝试解析损坏的 SSH 数据时抛出的错误。

### 第 2 步：定位数据处理逻辑

检查 [adapter.go#L180-L184](https://github.com/hanschad/session-proxy/blob/main/internal/protocol/adapter.go#L180-L184)：

```go
// Normal data - forward to reader
if _, err := a.writer.Write(agentMsg.Payload); err != nil {
    debugLog("Pipe Write Error: %v", err)
    return
}
```

**问题**：直接转发数据到 pipe，**没有按 SequenceNumber 重排序**。

当消息乱序到达时：
1. Seq=25 的数据应该在 Seq=24 之后，但它在 Seq=36 之后到达
2. 由于直接转发，SSH 数据流被打乱
3. SSH 协议解析失败，报 `max packet length exceeded`

### 第 3 步：参考 AWS 官方实现

AWS Agent/Plugin 使用消息缓冲和重排序机制：

[datachannel.go#L693-L747](https://github.com/aws/amazon-ssm-agent/blob/main/agent/session/datachannel/datachannel.go#L693-L747)：

```go
func (dataChannel *DataChannel) handleStreamDataMessage(...) {
    // 如果是期望的序列号，直接处理
    if streamDataMessage.SequenceNumber == dataChannel.ExpectedSequenceNumber {
        if err = dataChannel.processStreamDataMessage(log, streamDataMessage); err != nil {
            return err
        }
        dataChannel.ExpectedSequenceNumber++
        // 处理缓冲区中的后续消息
        return dataChannel.processIncomingMessageBufferItems(log)
    
    // 如果序列号大于期望值，缓冲等待
    } else if streamDataMessage.SequenceNumber > dataChannel.ExpectedSequenceNumber {
        dataChannel.AddDataToIncomingMessageBuffer(streamingMessage)
    
    // 如果序列号小于期望值（重复消息），忽略但发送 ACK
    } else {
        if err = dataChannel.SendAcknowledgeMessage(log, streamDataMessage); err != nil {
            return err
        }
    }
}
```

关键机制：
1. **ExpectedSequenceNumber**：跟踪期望的下一个序列号
2. **IncomingMessageBuffer**：缓冲乱序到达的消息
3. **processIncomingMessageBufferItems**：处理缓冲区中已就绪的消息

---

## 修复方案

### 修复 1：添加消息缓冲字段

[adapter.go#L47-L50](https://github.com/hanschad/session-proxy/blob/main/internal/protocol/adapter.go#L47-L50)：

```go
// Message reordering (per AWS protocol)
expectedSeqNum    int64                    // Next expected sequence number
incomingMsgBuffer map[int64]*AgentMessage  // Buffer for out-of-order messages
incomingMsgBufMu  sync.Mutex               // Protects incomingMsgBuffer
```

### 修复 2：实现消息处理方法

[adapter.go#L408-L462](https://github.com/hanschad/session-proxy/blob/main/internal/protocol/adapter.go#L408-L462)：

```go
// handleDataMessage processes data messages in sequence order.
func (a *Adapter) handleDataMessage(msg *AgentMessage) {
    seq := msg.Header.SequenceNumber

    a.incomingMsgBufMu.Lock()
    defer a.incomingMsgBufMu.Unlock()

    // Always send ACK (even for out-of-order or duplicate messages)
    if err := a.sendAck(msg); err != nil {
        debugLog("Ack Send Error: %v", err)
    }

    if seq == a.expectedSeqNum {
        // Message is in order - process it
        a.processMessage(msg)
        a.expectedSeqNum++
        // Check buffer for subsequent messages
        a.processBufferedMessages()

    } else if seq > a.expectedSeqNum {
        // Out of order - buffer it
        a.incomingMsgBuffer[seq] = msg

    } else {
        // seq < expectedSeqNum - duplicate/old message
        debugLog("Ignoring old message Seq=%d", seq)
    }
}
```

### 修复 3：初始化序列号时注意握手消息

**遇到的 Bug**：首次修复后仍然卡住，日志显示：

```
Buffering out-of-order message Seq=2 (expected=0)
Buffering out-of-order message Seq=3 (expected=0)
Buffering out-of-order message Seq=4 (expected=0)
```

**原因**：`expectedSeqNum` 卡在 0，因为 HandshakeRequest (Seq=0) 和 HandshakeComplete (Seq=1) 在单独的分支处理，没有更新 `expectedSeqNum`。

**修复**：在处理 HandshakeRequest 和 HandshakeComplete 后更新 `expectedSeqNum`：

[adapter.go#L166-L173](https://github.com/hanschad/session-proxy/blob/main/internal/protocol/adapter.go#L166-L173)：

```go
} else if err := a.handleHandshakeRequest(agentMsg); err != nil {
    debugLog("HandshakeRequest handling error: %v", err)
} else {
    // Successfully processed HandshakeRequest - update expected sequence
    a.incomingMsgBufMu.Lock()
    a.expectedSeqNum = agentMsg.Header.SequenceNumber + 1
    a.incomingMsgBufMu.Unlock()
}
```

---

## 验证结果

修复后日志显示消息按顺序处理：

```
Processing in-order message Seq=197
TX SSH Data: 36 bytes
Processing in-order message Seq=198
TX SSH Data: 36 bytes
```

SSH 连接稳定，SOCKS5 代理正常工作。

---

## 经验总结

1. **流式协议必须保序**：SSH 是流式协议，数据必须按顺序传输
2. **网络消息可能乱序**：WebSocket/TCP 虽然保序，但 SSM 协议层可能重发导致乱序
3. **参考官方实现**：AWS Agent 的消息缓冲机制是解决此问题的关键
4. **注意边界情况**：握手消息也有序列号，需要同步更新计数器

---

## 相关文件

| 文件 | 修改内容 |
|------|---------|
| [adapter.go](https://github.com/hanschad/session-proxy/blob/main/internal/protocol/adapter.go) | 添加消息缓冲和重排序逻辑 |
