# Duplicate HandshakeRequest 问题排查与修复

本文档记录了 SSM Agent 持续重发 HandshakeRequest 问题的完整排查和修复过程。

## 问题现象

运行 `session-proxy` 时，日志显示 AWS SSM Agent 持续重发相同的 `HandshakeRequest` 消息：

```
Received duplicate HandshakeRequest, resending ACK + Response
Received duplicate HandshakeRequest, resending ACK + Response
... (无限循环)
```

尽管 Proxy 已发送 ACK 和 HandshakeResponse，甚至 HandshakeComplete 也收到了，Agent 仍然不停重发。

---

## 排查过程

### 第 1 步：理解 Agent 的重发机制

通过分析 AWS Agent 源码 [datachannel.go](https://github.com/aws/amazon-ssm-agent/blob/main/agent/session/datachannel/datachannel.go)，发现：

1. Agent 发送消息后，将其添加到 `OutgoingMessageBuffer`
2. `ResendStreamDataMessageScheduler` 定期检查 Buffer，重发未被 ACK 的消息
3. 当收到 ACK 时，`ProcessAcknowledgedMessage` 通过 **SequenceNumber** 匹配并移除消息

```go
// datachannel.go:510-524
func (dataChannel *DataChannel) ProcessAcknowledgedMessage(...) {
    acknowledgeSequenceNumber := acknowledgeMessageContent.SequenceNumber
    for streamMessageElement := dataChannel.OutgoingMessageBuffer.Messages.Front(); ... {
        if streamMessage.SequenceNumber == acknowledgeSequenceNumber {
            dataChannel.RemoveDataFromOutgoingMessageBuffer(streamMessageElement)
            break
        }
    }
}
```

**关键发现**：Agent 仅通过 SequenceNumber 匹配，不验证 MessageId 格式。

### 第 2 步：确认 ACK 发送成功

添加详细日志确认 WebSocket 写入：

```go
debugLog("WebSocket WriteMessage OK: %d bytes, MsgType=%q", len(data), msg.Header.MessageType)
```

日志显示 `WebSocket WriteMessage OK`，确认 ACK 成功发送到 WebSocket 层。

### 第 3 步：排除格式问题 - MessageType 填充

检查 ACK 消息的二进制格式：

```
first 40: 0000007461636b6e6f776c6564676500000000000000000000000000000000000000000001
                    ^^^^^^^^^^^^^^^^^^^^^^^
                    "acknowledge" + null bytes (0x00)
```

对比 AWS 实现 [messageparser.go#L376-L396](https://github.com/aws/session-manager-plugin/blob/main/src/message/messageparser.go#L376-L396)：

```go
// AWS 使用空格填充
for i := offsetStart; i <= offsetEnd; i++ {
    byteArray[i] = ' '  // 0x20
}
```

**问题 1**：Proxy 使用 null bytes (0x00) 填充，AWS 使用 spaces (0x20)。

**修复** [message.go#L197-L206](https://github.com/hanschad/session-proxy/blob/main/internal/protocol/message.go#L197-L206)：

```go
var typeBytes [32]byte
for i := range typeBytes {
    typeBytes[i] = ' ' // Fill with spaces first
}
copy(typeBytes[:], m.Header.MessageType)
```

修复后仍然问题存在，继续排查。

### 第 4 步：发现根因 - UUID 字节顺序

深入分析 AWS 的 `putUuid` 函数 [messageparser.go#L415-L452](https://github.com/aws/session-manager-plugin/blob/main/src/message/messageparser.go#L415-L452)：

```go
func putUuid(log log.T, byteArray []byte, offset int, input uuid.UUID) (err error) {
    leastSignificantLong, _ := bytesToLong(log, input.Bytes()[8:16])  // 后 8 字节
    mostSignificantLong, _ := bytesToLong(log, input.Bytes()[0:8])    // 前 8 字节
    
    putLong(log, byteArray, offset, leastSignificantLong)       // 先写后 8 字节
    putLong(log, byteArray, offset+8, mostSignificantLong)      // 再写前 8 字节
}
```

**关键发现**：AWS 交换了 UUID 的高低位字节顺序！

Proxy 使用标准 `binary.Write` 直接写入 UUID，不交换顺序 → 导致二进制格式不匹配。

---

## 修复方案

### 修复 1：MessageType 填充

[message.go#L197-L206](https://github.com/hanschad/session-proxy/blob/main/internal/protocol/message.go#L197-L206)：

```diff
- var typeBytes [32]byte
+ var typeBytes [32]byte
+ for i := range typeBytes {
+     typeBytes[i] = ' ' // Fill with spaces first
+ }
  copy(typeBytes[:], m.Header.MessageType)
```

### 修复 2：UUID 序列化字节顺序

[message.go#L228-L239](https://github.com/hanschad/session-proxy/blob/main/internal/protocol/message.go#L228-L239)：

```go
// 7. MessageId (16 bytes) - AWS uses swapped byte order!
uuidBytes := m.Header.MessageId[:]
// Write LSB (bytes 8-16) first
if err := binary.Write(buf, binary.BigEndian, uuidBytes[8:16]); err != nil {
    return nil, err
}
// Write MSB (bytes 0-8) second
if err := binary.Write(buf, binary.BigEndian, uuidBytes[0:8]); err != nil {
    return nil, err
}
```

### 修复 3：UUID 反序列化字节顺序

[message.go#L312-L322](https://github.com/hanschad/session-proxy/blob/main/internal/protocol/message.go#L312-L322)：

```go
// 7. MessageId - AWS uses swapped byte order (LSB first, then MSB)
var lsb, msb [8]byte
if err := binary.Read(r, binary.BigEndian, &lsb); err != nil {
    return nil, err
}
if err := binary.Read(r, binary.BigEndian, &msb); err != nil {
    return nil, err
}
copy(h.MessageId[0:8], msb[:])
copy(h.MessageId[8:16], lsb[:])
```

### 修复 4：AcknowledgedMessageId 格式

[message.go#L132](https://github.com/hanschad/session-proxy/blob/main/internal/protocol/message.go#L132)：

```diff
- MessageId: CleanUUID(refMsgId),  // 无连字符
+ MessageId: refMsgId.String(),    // 带连字符 (与 AWS Plugin 一致)
```

---

## 验证结果

修复后日志不再显示持续的 `Received duplicate HandshakeRequest`，握手正常完成：

```
Received HandshakeRequest: ...
TX ACK for MsgId=xxx Seq=0
TX HandshakeResponse...
Received ACK: ... Seq=0
Received HandshakeComplete: ...
SSH handshake successful
SOCKS5 proxy listening on :28881
```

---

## 经验总结

1. **二进制协议调试**：添加 hex dump 日志，逐字节对比官方实现
2. **阅读源码**：AWS Agent/Plugin 源码是权威参考
3. **UUID 处理**：不同库的 UUID 二进制格式可能不同，需注意字节顺序
4. **填充字符**：字符串字段的填充字符（null vs space）可能影响解析

---

## 相关文件

| 文件 | 修改内容 |
|------|---------|
| [message.go](https://github.com/hanschad/session-proxy/blob/main/internal/protocol/message.go) | MessageType 填充、UUID 字节顺序 |
| [adapter.go](https://github.com/hanschad/session-proxy/blob/main/internal/protocol/adapter.go) | 调试日志 |
