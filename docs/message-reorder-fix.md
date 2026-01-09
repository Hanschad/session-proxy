# Message Reorder Fix

SSH 数据传输阶段 `ssh: max packet length exceeded` 错误的根因与修复。

## 问题

SSH 连接建立成功，但传输数据时突然中断：
```
ssh: max packet length exceeded
```

## 根因

SSM 消息乱序到达（Seq=25 在 Seq=36 之后），Proxy 直接转发数据导致 SSH 流被打乱。

```
RX Frame: Seq=36
RX Frame: Seq=25  ← 乱序
ssh: max packet length exceeded
```

AWS Agent 使用 `expectedSeqNum` + `incomingMsgBuffer` 实现消息重排序，Proxy 缺失此机制。

## 修复

### 添加缓冲字段 (`adapter.go`)

```go
expectedSeqNum    int64                    // 期望的下一个序列号
incomingMsgBuffer map[int64]*AgentMessage  // 乱序消息缓冲
```

### 消息处理逻辑

```go
func (a *Adapter) handleDataMessage(msg *AgentMessage) {
    seq := msg.Header.SequenceNumber
    
    if seq == a.expectedSeqNum {
        // 按序 - 直接处理
        a.processMessage(msg)
        a.expectedSeqNum++
        a.processBufferedMessages()  // 检查缓冲区
    } else if seq > a.expectedSeqNum {
        // 乱序 - 缓冲
        a.incomingMsgBuffer[seq] = msg
    }
    // seq < expected: 重复消息，忽略
}
```

## 注意事项

握手消息也有序列号，处理后需更新 `expectedSeqNum`：
```go
a.expectedSeqNum = agentMsg.Header.SequenceNumber + 1
```

## 教训

1. 流式协议 (SSH) 必须保序
2. SSM 协议层可能导致消息乱序
3. 参考 AWS Agent 的 `handleStreamDataMessage` 实现
