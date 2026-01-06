# HandshakeRequest 持续重发问题调查报告

## 问题概述

AWS SSM Agent 在收到 HandshakeResponse 后仍然持续重发相同的 HandshakeRequest 消息，持续约 27 分钟，最终导致远端关闭通道。尽管本地服务正确发送了 HandshakeResponse 和相应的 ACK，但 Agent 未能正确识别并完成握手流程。

## 现象描述

### 时间线
1. **初始握手** (日志前几行): Proxy 收到 HandshakeRequest，发送 ACK 和 HandshakeResponse
2. **重复发送** (27分钟): Agent 持续重发相同 MsgId 的 HandshakeRequest
3. **通道关闭** (日志行 ~57431): 远端关闭通道，显示 "Channel closed by remote"
4. **服务异常** (后续): SOCKS5 proxy 继续运行但所有连接失败 "read/write on closed pipe"

### 日志特征
```
duplicate HandshakeRequest, ignoring. MsgId: <same-id>
channel_closed: {"MessageSchemaVersion":"1.0","MessageId":"<id>","DestinationId":"<agent-id>",...}
Closing Adapter
connect failed: dial unix /tmp/ssh-socks-proxy.sock: read/write on closed pipe
```

## 代码分析

### 1. Proxy 端实现 (session-proxy)

#### HandshakeRequest 处理
**文件**: `internal/protocol/adapter.go` (行 132-165)

```go
case message.HandshakeRequest:
    if a.handshakeResponded {
        log.Printf("duplicate HandshakeRequest, ignoring. MsgId: %s", msg.MessageId)
        ackMsg := message.NewAcknowledgeMessage(msg)
        if err := a.sendMessage(ackMsg); err != nil {
            log.Printf("Error sending ACK for duplicate handshake: %v", err)
        }
        continue
    }
    
    a.handshakeResponded = true
    ackMsg := message.NewAcknowledgeMessage(msg)
    if err := a.sendMessage(ackMsg); err != nil {
        return fmt.Errorf("failed to send handshake ACK: %w", err)
    }
    
    responseMsg := a.buildHandshakeResponse(msg)
    if err := a.sendMessage(responseMsg); err != nil {
        return fmt.Errorf("failed to send handshake response: %w", err)
    }
```

**关键行为**:
- 首次收到 HandshakeRequest: 发送 ACK + HandshakeResponse
- 后续收到重复 HandshakeRequest: 仅发送 ACK，不重复发送 HandshakeResponse
- 使用 `handshakeResponded` 标志位防止重复响应

#### HandshakeResponse 构造
**文件**: `internal/protocol/adapter.go` (行 240-277)

```go
func (a *Adapter) buildHandshakeResponse(req *message.AgentMessage) *message.AgentMessage {
    response := &message.AgentMessage{
        MessageType:    message.OutputStreamMessage,
        SchemaVersion:  1,
        CreatedDate:    uint64(time.Now().UnixMilli()),
        MessageId:      uuid.New().String(),
        PayloadType:    uint32(message.Output),
        SequenceNumber: a.nextOutSeq(),
    }
    
    payload := map[string]interface{}{
        "action": "SessionType",
        "ProcessedClientActions": []map[string]interface{}{
            {
                "ActionType":   "SessionType",
                "ActionStatus": 1, // Success
            },
        },
        "SessionId":      req.MessageId,
        "SessionType":    "Port",
        "SessionProperties": map[string]interface{}{
            "type": "LocalPortForwarding",
        },
    }
    
    payloadBytes, _ := json.Marshal(payload)
    response.Payload = payloadBytes
    return response
}
```

**响应格式**: 完全符合 AWS Session Manager 协议要求
- `ActionStatus: 1` 表示成功
- `ProcessedClientActions` 数组包含 SessionType 处理结果
- 包含 SessionType 和 SessionProperties

### 2. Agent 端实现 (amazon-ssm-agent)

#### HandshakeRequest 发送
**文件**: `agent/session/datachannel/datachannel.go` (行 955-1020)

```go
func (dataChannel *DataChannel) PerformHandshake(...) error {
    responseChan := make(chan bool)
    dataChannel.wsChannel.SetChannelHandlers(responseChan)
    
    clientHandshakePayload := buildHandshakeRequest(...)
    
    handshakeRequest := &mgsContracts.AgentMessage{
        MessageType:    mgsContracts.InputStreamDataMessage,
        SequenceNumber: 0,
        Flags:          handshakeRequestFlag,
        MessageId:      uuid.NewV4(),
        Payload:        clientHandshakePayload,
    }
    
    if err := dataChannel.SendMessage(handshakeRequest, websocket.BinaryMessage); err != nil {
        return err
    }
    
    // Wait for response or timeout
    select {
    case <-responseChan:
        return nil
    case <-time.After(handshakeTimeout):
        return errors.New("handshake timeout")
    }
}
```

**关键逻辑**:
- 发送 HandshakeRequest 后等待 `responseChan` 信号
- 如果超时 (`handshakeTimeout`), 返回错误
- 依赖 `handleHandshakeResponse` 向 `responseChan` 发送信号

#### HandshakeResponse 处理
**文件**: `agent/session/datachannel/datachannel.go` (行 860-894)

```go
func (dataChannel *DataChannel) handleHandshakeResponse(output mgsContracts.AgentMessage) error {
    var handshakeResponse mgsContracts.HandshakeResponsePayload
    
    if err := json.Unmarshal(output.Payload, &handshakeResponse); err != nil {
        return err
    }
    
    if len(handshakeResponse.ProcessedClientActions) > 0 {
        for _, action := range handshakeResponse.ProcessedClientActions {
            if action.ActionStatus == mgsContracts.Success {
                dataChannel.pause = false
            } else {
                dataChannel.pause = true
                dataChannel.skipHandshake = true
            }
            
            if action.ActionType == mgsContracts.SessionType {
                if action.ActionStatus == mgsContracts.Success {
                    close(dataChannel.handshakeResponseChan)
                }
            }
        }
    }
    
    return nil
}
```

**触发条件**:
- 必须成功解析 JSON Payload
- 检查 `ProcessedClientActions` 数组
- 当 `ActionType == "SessionType"` 且 `ActionStatus == 1` 时关闭 `handshakeResponseChan`

#### 消息重传机制
**文件**: `agent/session/datachannel/datachannel.go` (ResendStreamDataMessageScheduler)

```go
func (dataChannel *DataChannel) ResendStreamDataMessageScheduler(context) {
    for {
        select {
        case <-time.After(resendSleepInterval):
            dataChannel.outgoingMessageBuffer.Messages.Range(func(key, value interface{}) bool {
                message := value.(*mgsContracts.AgentMessage)
                if time.Since(message.CreatedDate) > roundTripTimeConst {
                    dataChannel.SendMessage(message, websocket.BinaryMessage)
                }
                return true
            })
        }
    }
}
```

**重传逻辑**:
- 定期检查 `outgoingMessageBuffer` 中未确认的消息
- 如果消息发送后经过 `roundTripTimeConst` 仍未收到 ACK，则重发
- 这解释了为什么 HandshakeRequest 会被持续重发

## 根本原因分析

### 可能原因 1: ACK 机制问题
**假设**: Agent 未收到 HandshakeRequest 的 ACK

**分析**:
- Proxy 代码确实发送了 ACK (`NewAcknowledgeMessage`)
- 但如果 ACK 未正确发送或 Agent 未正确接收，HandshakeRequest 会留在 `outgoingMessageBuffer` 中
- 重传调度器会持续重发该消息

**验证点**:
- Proxy 的 `sendMessage` 方法是否成功发送 ACK
- WebSocket 连接是否稳定
- Agent 端是否正确处理 ACK 并从 buffer 中移除消息

### 可能原因 2: HandshakeResponse 未被正确处理
**假设**: Agent 收到了 HandshakeResponse 但未能正确解析或识别

**分析**:
- Proxy 发送的 HandshakeResponse 格式看起来正确
- 但 Agent 的 `handleHandshakeResponse` 可能因为以下原因失败:
  - JSON 解析失败
  - PayloadType 不匹配 (Proxy 使用 `Output`, Agent 期望特定类型)
  - 消息路由问题 (消息未到达 `handleHandshakeResponse`)

**关键差异**:
```go
// Proxy 设置
response.PayloadType = uint32(message.Output)  // Output = 1
response.MessageType = message.OutputStreamMessage

// Agent 期望
// 可能期望特定的 MessageType 或 PayloadType 才会路由到 handleHandshakeResponse
```

### 可能原因 3: 消息序列号或标志位问题
**假设**: Agent 根据消息的特定属性来识别 HandshakeResponse

**分析**:
- Agent 的 HandshakeRequest 使用 `Flags: handshakeRequestFlag`
- HandshakeResponse 可能需要特定的 Flags 或其他标识符
- Proxy 的响应可能缺少这些标识符，导致 Agent 无法将其识别为握手响应

### 可能原因 4: SequenceNumber 管理问题
**假设**: Agent 期望特定的序列号管理方式

**分析**:
- Proxy 使用 `nextOutSeq()` 递增序列号
- Agent 可能期望 HandshakeResponse 使用序列号 0 或与 HandshakeRequest 相同的序列号
- 序列号不匹配可能导致消息被忽略

## 后续影响

### 1. 通道关闭但服务未终止
**问题**: 远端关闭通道后，本地服务仍在运行

**原因**:
- Adapter 的 `readLoop` 检测到通道关闭并关闭了 adapter
- 但 `main.go` 中的 context 未被取消
- SOCKS5 proxy 继续监听，但底层通道已关闭

**代码位置**: `cmd/session-proxy/main.go` (行 71-74)
```go
defer func() {
    log.Println("Closing adapter...")
    adapter.Close()  // 仅关闭 adapter，未取消 context
}()
```

### 2. 缺乏重连机制
**问题**: 通道断开后无自动重连尝试

**现状**:
- 代码中没有检测通道断开并重新建立 WebSocket 连接的逻辑
- 一旦通道关闭，整个 session 终止

## 推荐行动

### 短期 (调试验证)
1. **添加详细日志**: 在 `sendMessage` 中记录每条消息的发送结果，特别是 ACK
2. **验证 ACK 发送**: 确认 ACK 是否成功通过 WebSocket 发送
3. **监控 Agent 日志**: 查看 Agent 端是否有 HandshakeResponse 处理失败的日志
4. **抓包分析**: 使用 Wireshark 捕获 WebSocket 流量，确认消息内容和顺序

### 中期 (问题修复)
1. **对齐消息格式**: 仔细对比 Agent 官方实现的 HandshakeResponse 格式
   - 检查是否需要特定的 Flags
   - 验证 PayloadType 和 MessageType 的正确组合
   - 确认 SequenceNumber 的期望值

2. **改进 ACK 机制**: 确保所有 ACK 都能可靠发送
   - 添加发送确认
   - 实现重试机制

3. **实现生命周期管理**: 
   - 在 adapter 关闭时取消主 context
   - 优雅关闭所有依赖服务 (SSH, SOCKS5)

### 长期 (架构改进)
1. **实现重连逻辑**:
   - 检测通道断开
   - 实现指数退避重连
   - 维护会话状态

2. **增强监控和告警**:
   - 握手超时告警
   - 通道健康检查
   - 自动故障恢复

3. **参考官方实现**: 考虑使用或借鉴 `amazon-ssm-agent` 的 datachannel 实现，确保完全兼容

## 相关文件

### Proxy 实现
- `internal/protocol/adapter.go`: 核心握手逻辑
- `cmd/session-proxy/main.go`: 主服务入口和生命周期管理
- `internal/protocol/message/message.go`: 消息定义

### Agent 实现
- `agent/session/datachannel/datachannel.go`: 握手和重传逻辑
- `agent/session/communicator/websocketchannel.go`: WebSocket 通道实现

### 已有文档
- `docs/troubleshooting-ssm-handshake.md`: 握手问题排查
- `docs/architecture.md`: 系统架构说明
- `docs/SESSION_MANAGER_CONNECTION_ANALYSIS.md`: 连接分析

## 更新日期
2026-01-05

---

## 深度分析 (2026-01-06 更新)

基于对 AWS 官方源码的深入分析，我找到了问题的根本原因。

### 📂 分析的官方源码

| 组件 | 文件路径 | 角色 |
|------|---------|------|
| Agent | `aws/amazon-ssm-agent/agent/session/datachannel/datachannel.go` | 服务端（EC2实例上运行） |
| Plugin | `aws/session-manager-plugin/src/datachannel/streaming.go` | 客户端（用户机器上运行） |

### 🔴 根因 1：重复 HandshakeRequest 时未重发 Response

**官方 Plugin 的处理逻辑** (`streaming.go` 行 616-631):
```go
// 当收到 HandshakeRequest 时
case message.HandshakeRequestPayloadType:
    if err = SendAcknowledgeMessageCall(log, dataChannel, outputMessage); err != nil {
        return err
    }
    // 每次都调用 handleHandshakeRequest 来发送 Response
    if err = dataChannel.handleHandshakeRequest(log, outputMessage); err != nil {
        return err
    }
```

**Proxy 当前实现** (`adapter.go` 行 148-155):
```go
if isDuplicate || a.handshakeResponded {
    debugLog("Skipping duplicate HandshakeRequest, sending ACK only")
    if err := a.sendAck(agentMsg); err != nil {
        debugLog("Ack Send Error: %v", err)
    }
    // ❌ 没有重发 HandshakeResponse！
}
```

**问题**: 当 Proxy 收到重复的 HandshakeRequest（Agent 重传），只发送 ACK 而**不重发 HandshakeResponse**。如果第一次的 HandshakeResponse 丢失，Agent 将永远收不到它。

### 🔴 根因 2：HandshakeResponse 应该被添加到 Outgoing Buffer

**官方 Plugin** 使用 `SendInputDataMessage` 发送 HandshakeResponse (`streaming.go` 行 570):
```go
if err := dataChannel.SendInputDataMessage(log, message.HandshakeResponsePayloadType, resultBytes); err != nil {
    return err
}
```

`SendInputDataMessage` 会将消息添加到 `OutgoingMessageBuffer` (行 320-327):
```go
streamingMessage := StreamingMessage{
    msg,
    dataChannel.StreamDataSequenceNumber,
    time.Now(),
    new(int),
}
dataChannel.AddDataToOutgoingMessageBuffer(streamingMessage)
dataChannel.StreamDataSequenceNumber = dataChannel.StreamDataSequenceNumber + 1
```

**Proxy 当前实现**: 发送 HandshakeResponse 后没有保存到 buffer，因此：
1. 没有重传机制
2. 如果消息丢失，无法恢复

### 🔴 根因 3：缺少 Resend Scheduler

**官方 Plugin** 有 `ResendStreamDataMessageScheduler` (`streaming.go` 行 334-363):
```go
func (dataChannel *DataChannel) ResendStreamDataMessageScheduler(log log.T) (err error) {
    go func() {
        for {
            time.Sleep(config.ResendSleepInterval)
            streamMessageElement := dataChannel.OutgoingMessageBuffer.Messages.Front()
            if streamMessageElement == nil {
                continue
            }
            streamMessage := streamMessageElement.Value.(StreamingMessage)
            if time.Since(streamMessage.LastSentTime) > dataChannel.RetransmissionTimeout {
                // 重发消息
                if err = SendMessageCall(log, dataChannel, streamMessage.Content, websocket.BinaryMessage); err != nil {
                    log.Errorf("Unable to send stream data message: %s", err)
                }
                streamMessage.LastSentTime = time.Now()
            }
        }
    }()
    return
}
```

**Proxy**: 没有实现重传调度器。

---

## 🛠 修复方案

### 修复 1：收到重复 HandshakeRequest 时也重发 Response

```diff
--- a/internal/protocol/adapter.go
+++ b/internal/protocol/adapter.go
@@ -148,10 +148,12 @@ func (a *Adapter) readLoop() {
         case PayloadTypeHandshakeRequest:
             debugLog("Received HandshakeRequest: %s", string(agentMsg.Payload))
             if isDuplicate || a.handshakeResponded {
-                debugLog("Skipping duplicate HandshakeRequest, sending ACK only")
+                debugLog("Received duplicate HandshakeRequest, resending ACK + Response")
                 if err := a.sendAck(agentMsg); err != nil {
                     debugLog("Ack Send Error: %v", err)
                 }
+                // 也重发 HandshakeResponse
+                a.resendHandshakeResponse()
             } else if err := a.handleHandshakeRequest(agentMsg); err != nil {
                 debugLog("HandshakeRequest handling error: %v", err)
             }
```

### 修复 2：添加 resendHandshakeResponse 方法

```go
// resendHandshakeResponse 重发上次构建的 HandshakeResponse
func (a *Adapter) resendHandshakeResponse() error {
    a.writeMu.Lock()
    if a.lastHandshakeResponse == nil {
        a.writeMu.Unlock()
        return nil
    }
    response := a.lastHandshakeResponse
    a.writeMu.Unlock()
    
    debugLog("TX HandshakeResponse (resend)")
    return a.writeMessage(response)
}
```

### 修复 3：保存 HandshakeResponse 以便重发

在 `handleHandshakeRequest` 中保存 response:
```go
func (a *Adapter) handleHandshakeRequest(orig *AgentMessage) error {
    // ... 构建 responseMsg ...
    
    // 保存以便重发
    a.lastHandshakeResponse = responseMsg
    
    return a.writeMessage(responseMsg)
}
```

### 修复 4：生命周期管理

在 `main.go` 中实现优雅关闭:
```go
// 当 adapter 关闭时取消 context
adapterDone := make(chan struct{})
go func() {
    <-adapter.Done() // 需要在 Adapter 中添加此 channel
    cancel()
    close(adapterDone)
}()
```

---

## 验证步骤

1. **启用 DEBUG 日志**: `DebugMode = true`
2. **运行测试**: 建立 SSH 连接
3. **预期行为**: 
   - 首次 HandshakeRequest → ACK + Response
   - 重复 HandshakeRequest → ACK + Response (重发)
   - HandshakeComplete → 握手完成
4. **成功指标**: 无 "duplicate HandshakeRequest" 持续出现，连接在 5 秒内完成握手

---

## 附录：官方实现关键代码位置

| 功能 | Agent 位置 | Plugin 位置 |
|------|-----------|-------------|
| 发送 HandshakeRequest | `datachannel.go:1068-1081` | N/A |
| 处理 HandshakeRequest | N/A | `streaming.go:434-492` |
| 发送 HandshakeResponse | N/A | `streaming.go:561-574` |
| 处理 HandshakeResponse | `datachannel.go:860-896` | N/A |
| ACK 发送 | `datachannel.go:527-548` | `streaming.go:382-402` |
| 消息重传调度器 | `datachannel.go:475-508` | `streaming.go:334-363` |
| 处理 ACK | `datachannel.go:750-761` | `streaming.go:761-774` |
