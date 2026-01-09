# Handshake Fix

SSM Agent 持续重发 HandshakeRequest 问题的根因与修复。

## 问题

Agent 无限重发相同的 HandshakeRequest，尽管 Proxy 已发送 ACK 和 HandshakeResponse。

## 根因

**1. MessageType 填充字符错误**

| 实现 | 填充 |
|------|------|
| AWS | 空格 `0x20` |
| Proxy (错误) | NULL `0x00` |

**2. UUID 字节顺序错误**

AWS 交换 UUID 高低位字节：
```go
// AWS putUuid: 先写 bytes[8:16]，再写 bytes[0:8]
putLong(byteArray, offset, leastSignificantLong)   // bytes 8-16
putLong(byteArray, offset+8, mostSignificantLong)  // bytes 0-8
```

Proxy 使用标准 BigEndian，导致消息无法匹配。

## 修复

### MessageType 填充 (`message.go`)

```go
var typeBytes [32]byte
for i := range typeBytes {
    typeBytes[i] = ' '  // 空格填充
}
copy(typeBytes[:], m.Header.MessageType)
```

### UUID 序列化 (`message.go`)

```go
// Write LSB (bytes 8-16) first, then MSB (bytes 0-8)
binary.Write(buf, binary.BigEndian, uuidBytes[8:16])
binary.Write(buf, binary.BigEndian, uuidBytes[0:8])
```

## 教训

1. 二进制协议需逐字节对比官方实现
2. AWS Agent/Plugin 源码是权威参考
3. 字符串填充字符 (null vs space) 影响解析
