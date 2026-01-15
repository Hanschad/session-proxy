# Large Transfer Stability (Docker Push) over SOCKS5 + SSH-over-SSM

本文总结 `session-proxy` 在高并发/大流量场景（典型：`docker push` 通过 SOCKS5 代理上传镜像层）出现的连接不稳定问题、排查路径、根因定位与最终修复方案。

目标：
- 大文件/大层（MB~GB）上传不再出现 `use of closed network connection` / layer retry。
- 并发连接不再因单个大上传而“卡死/阻塞”。
- 启动与连接维护在网络抖动下具备可用性与可观测性。

## 1. 典型问题现象

### 1.1 Docker push 失败（核心）
客户端表现通常是 layer pushing 到某个进度后重试，最终报错：
- `use of closed network connection`
- `write: broken pipe`
- 或大量 layer `Retrying in N seconds`

在 `session-proxy` 侧（debug.log）常伴随：
- `channel_closed by remote`
- WebSocket abnormal close（例如 `unexpected EOF` / close 1006）
- `WebSocket WriteMessage FAILED: use of closed network connection`
- SOCKS5 连接关闭：`socks: closed ... up_err=writeto ... use of closed network connection`

### 1.2 并发阻塞/启动慢
- 启动阶段连接池创建过慢（串行建连导致启动时间线性增长）。
- 大上传进行中，其他连接的 `dial` 或数据转发被放大阻塞，表现为“新连接卡住”。

## 2. 排查与增强路径（按演进顺序）

> 这里不是单点改动能解决的问题，最终收敛为：协议正确性 + 可靠传输 + 流控 + 连接池可用性 + 可观测性。

### 2.1 先补齐可观测性（必须）
为了能把 Docker 的错误与代理内部事件对齐，加入了：
- SOCKS5 每连接 connID、目标地址、传输字节数、持续时间、上下行错误日志。
- upstream dial 日志：connID 选用哪条 sshConn、耗时、inflight/active。
- WebSocket 写失败日志与 `channel_closed` payload 打印（用于确认 remote 是否主动关闭）。

这些改动的直接收益：
- 能确定错误并非 Docker 端随机，而是 upstream（SSM/MGS）主动关闭 channel。
- 能看到失败与“发送过快/积压过大/flow-control 触发”的强相关。

### 2.2 修复协议正确性（否则后续都是无效功）
历史问题中曾出现：
- HandshakeRequest 被无限重发。
- SSH 流出现 `ssh: max packet length exceeded`（典型乱序/重复导致的流损坏）。

对应修复已在：
- `docs/handshake-fix.md`：MessageType padding（space vs null）+ UUID 字节序（AWS swapped LSB/MSB）。
- `docs/message-reorder-fix.md`：expectedSeqNum + incomingMsgBuffer 保序。

这些属于“协议正确性底座”。

### 2.3 连接池与维护（降低抖动/阻塞）
在高并发场景下，单条 SSM+SSH 通道会成为瓶颈：
- 多路 SOCKS 连接争用同一 sshConn 时容易触发远端关闭或长尾延迟。
- 连接重建/维护若做得不好，会造成 session churn（频繁 StartSession）进一步放大问题。

主要措施：
- 引入 ssh.Client 池与容量感知的选择策略（尽量把新连接分散到空闲 sshConn）。
- 并行建连以降低启动时间。
- 维护策略与参数调优（限制扩容速度/并行度，避免“补池风暴”）。

#### 2.3.1 Dial 超时导致的资源泄漏（SSH channel leak）
早期实现为了支持 ctx 超时，会用 goroutine + select 来包一层 dial。问题在于：
- ctx 先超时返回时，dial goroutine 仍可能在稍后成功拿到 conn。
- 如果此时不 Close，这个 SSH channel 会“泄漏”，占用窗口并让后续 dial 更容易卡住/超时。

修复思路：
- dial goroutine 返回成功 conn 时，如果发现 ctx 已 Done，则立即 `conn.Close()`，确保不会泄漏。

#### 2.3.2 activeChannels 语义修正 + 最小负载选路
为了解决“大上传占用某条 sshConn 导致新连接卡住”的头阻塞：
- 把 activeChannels 的语义从“dial 并发数”修正为“已返回的 net.Conn 生命周期占用数”。
- 通过包装 net.Conn，在 `Close()` 时做 activeChannels -1（避免只统计 dial 阶段）。
- 选路从 round-robin 改为最小负载优先（按 activeChannels，其次 inflightDials），让新连接更倾向落到空闲 sshConn。
- 增加每条 sshConn 的容量上限（例如 `maxChannelsPerConn`），防止无限 multiplex。

#### 2.3.3 强制重连的爆炸半径控制
早期策略是：短时间多次 dial timeout 触发“全量重连/清空池”。这会直接中断正在上传的长连接。

修复思路：
- 将“重连/驱逐”粒度下沉到单条 sshConn：只移除发生问题的连接，让维护协程补齐池。
- 对强制驱逐加 cooldown，避免抖动时反复重建导致更大不稳定。

### 2.4 关键根因：MGS 流控消息 pause_publication
通过日志发现 MGS 会发送：
- `pause_publication`

而早期实现会忽略该消息，导致在 MGS 要求暂停时仍持续发送，随后出现：
- `channel_closed`（remote 主动关闭）

对应修复：
- Adapter 支持 `pause_publication` / `start_publication`。
- `pause_publication` 时阻塞后续 Write（通过 wait channel），避免继续发送。

但仅有 pause/resume 仍不足以完全解决大文件上传：
- 因为 TCP->SSM 的桥接会产生大量“未 ACK 的在途数据”，在 pause 之前已经积压过大。

## 3. 最终收敛的稳定性方案（解决大文件上传）

### 3.1 补齐“可靠发送”语义：Outgoing buffer + ACK tracking + 重传
参考 amazon-ssm-agent 的 datachannel 机制：
- 发送方需要保存“已发送但未确认（ACK）”的消息。
- 收到 ACK 后才能从 buffer 中移除。
- 超时未 ACK 的 oldest 消息需要重发（scheduler）。

在 `session-proxy` 的 Adapter 中实现：
- 解析 `acknowledge` 的 JSON payload（`AcknowledgedMessageType/Id/SequenceNumber`）。
- 维护 outgoing map（key=seq），记录 msgID、二进制 frame、lastSent。
- 后台 `resendLoop`（100ms tick）仅重发 oldest 未 ACK 消息。

这样做的意义：
- 网络抖动/丢包时不会直接造成 SSH/TLS 流中断。
- 与 MGS 的滑动窗口/ACK 语义对齐，避免无控制地“盲发”。

### 3.2 关键稳定开关：对 TCP 写入施加 backpressure（限制未 ACK 在途窗口）
即使实现 ACK/retransmit，如果 TCP 写入速度远高于 ACK 返回速度，outgoing 会无限增长：
- 内存增长（风险）。
- MGS/远端会因为积压过大触发 pause/甚至直接 `channel_closed`。

因此引入：
- `SESSION_PROXY_SSM_MAX_UNACKED_BYTES`：限制“未 ACK 的二进制 frame 总量”。
- 超出限制时，Adapter.Write 阻塞等待（直到 ACK 释放窗口）。

实践结论：
- 对 docker push，必须把发送窗口控制到较小范围，才能避免 MGS 主动关闭。

### 3.3 Payload 分片大小必须保守（与 Agent 默认一致）
在多轮实验中发现：
- `SESSION_PROXY_SSM_CHUNK_SIZE=8192` 即使配合窗口限制，仍容易触发 `channel_closed`。
- `SESSION_PROXY_SSM_CHUNK_SIZE=1024`（与 amazon-ssm-agent 的 `StreamDataPayloadSize` 一致）可稳定完成大文件上传。

因此代码层将 SSM stream chunk 限制在 `<=1024`，默认 `1024`。

## 4. 验证结果（A/B 实验）

- 方案A：chunk=8192 + max_unacked_bytes=262144 失败（出现 pause_publication + channel_closed）。
- 方案B：chunk=1024 + max_unacked_bytes=262144 成功上传大文件。

代码当前默认值已对齐方案B（无需显式设置 env var 也能获得同等行为）。

## 5. 运维/调参建议

### 5.1 推荐默认（稳定优先）
- `chunk=1024`
- `max_unacked_bytes=262144`（256KB）

### 5.2 如需调优
- 在高 RTT 场景可适当增加 `SESSION_PROXY_SSM_MAX_UNACKED_BYTES`，但若出现 `channel_closed`，应立即下调。
- 不建议把 `SESSION_PROXY_SSM_CHUNK_SIZE` 调大于 1024（已在代码层限制）。

### 5.3 排障关键日志
- `Outgoing buffer full ... backpressuring`：说明 ACK 跟不上发送。
- `pause_publication` / `Publication paused by remote`：说明 MGS 触发流控。
- `channel_closed by remote`：远端主动断开。

## 6. 代码落点（便于回溯）

- `internal/protocol/adapter.go`
  - pause/start publication 处理
  - outgoing ACK tracking
  - resendLoop
  - max unacked window/backpressure
- `internal/socks5/server.go`
  - per-conn 统计日志（bytes/duration/errors）
- `internal/upstream/pool.go`
  - 池化、容量感知选路、维护/扩容策略

## 7. 总结

本次稳定性问题的本质是：
- SSM/MGS 通道并非无限吞吐的 TCP；它具有明确的 ACK/流控语义。
- 把 SOCKS/TCP 数据“直接高速写进”SSM 通道会触发远端保护（pause_publication/channel_closed）。

最终通过：
- 协议正确性修复（padding/uuid/乱序）
- 可靠发送（outgoing+ACK+重传）
- 流控处理（pause_publication）
- 发送窗口限制（backpressure）
- 保守 payload 分片（1024）

实现了可稳定完成 Docker 大文件上传的代理行为，并同时提升了可观测性与可维护性。
