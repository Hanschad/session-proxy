# AWS Session Manager 三端建连过程代码走读

> 本文档详细分析 aws-cli、session-manager-plugin、amazon-ssm-agent 三个项目协作建立 Session Manager 连接的完整过程。

---

## 目录

1. [概述与架构](#1-概述与架构)
2. [AWS CLI 入口分析](#2-aws-cli-入口分析)
3. [Plugin 入口与 Session 初始化](#3-plugin-入口与-session-初始化)
4. [Agent 控制通道](#4-agent-控制通道)
5. [Plugin 端 DataChannel 初始化](#5-plugin-端-datachannel-初始化)
6. [Agent 端数据通道](#6-agent-端数据通道)
7. [WebSocket 连接建立详解](#7-websocket-连接建立详解)
8. [握手协议详解](#8-握手协议详解)
9. [KMS 加密协商](#9-kms-加密协商)
10. [消息协议和序列化](#10-消息协议和序列化)
11. [消息确认与重传](#11-消息确认与重传)
12. [会话处理器](#12-会话处理器)
13. [错误处理与重连](#13-错误处理与重连)
14. [配置参数汇总](#14-配置参数汇总)

---

## 1. 概述与架构

### 1.1 涉及的三个项目

| 项目 | 语言 | 运行位置 | 主要职责 |
|------|------|----------|----------|
| **aws-cli** | Python | 用户本地 | 命令行入口，调用 StartSession API，启动 plugin 子进程 |
| **session-manager-plugin** | Go | 用户本地 | WebSocket 客户端，与 MGS 通信，处理终端 I/O |
| **amazon-ssm-agent** | Go | EC2 实例 | WebSocket 客户端，与 MGS 通信，执行 shell 命令 |

### 1.2 核心服务

- **SSM (Systems Manager)**: AWS 托管服务，提供 StartSession API
- **MGS (Message Gateway Service)**: AWS 托管的 WebSocket 网关服务（ssmmessages 端点）

### 1.3 三端整体架构图

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                              AWS Session Manager 架构                                │
└─────────────────────────────────────────────────────────────────────────────────────┘

    用户本地                           AWS Cloud                         EC2 实例
┌──────────────────┐           ┌─────────────────────┐           ┌──────────────────┐
│                  │           │                     │           │                  │
│   ┌──────────┐   │           │   ┌─────────────┐   │           │   ┌──────────┐   │
│   │ aws-cli  │   │  HTTPS    │   │     SSM     │   │           │   │   SSM    │   │
│   │ (Python) │───┼──────────>│   │   Service   │   │           │   │  Agent   │   │
│   └────┬─────┘   │           │   └─────────────┘   │           │   │  (Go)    │   │
│        │         │           │                     │           │   └────┬─────┘   │
│   subprocess     │           │   ┌─────────────┐   │           │        │         │
│        │         │           │   │    MGS      │   │           │        │         │
│   ┌────▼─────┐   │ WebSocket │   │ (Message    │   │ WebSocket │   ┌────▼─────┐   │
│   │ session- │   │  (wss://) │   │  Gateway    │   │  (wss://) │   │  Data    │   │
│   │ manager- │───┼──────────>│   │  Service)   │<──┼───────────┼───│ Channel  │   │
│   │ plugin   │   │           │   │             │   │           │   │          │   │
│   │  (Go)    │<──┼───────────┼───│ ssmmessages │───┼──────────>│   │          │   │
│   └──────────┘   │           │   └─────────────┘   │           │   └──────────┘   │
│                  │           │                     │           │                  │
└──────────────────┘           └─────────────────────┘           └──────────────────┘
```

### 1.4 关键代码文件对应关系

```
aws-cli/
└── awscli/customizations/sessionmanager.py     # CLI 入口，启动会话

session-manager-plugin/src/
├── sessionmanagerplugin-main/main.go           # Plugin 入口
├── sessionmanagerplugin/session/
│   ├── session.go                              # Session 初始化和验证
│   └── sessionhandler.go                       # DataChannel 打开和握手处理
├── datachannel/streaming.go                    # 数据通道实现
├── communicator/websocketchannel.go            # WebSocket 通道
├── websocketutil/websocketutil.go              # WebSocket 工具
├── message/clientmessage.go                    # 消息结构定义
├── message/messageparser.go                    # 消息序列化
└── encryption/encrypter.go                     # 加密实现

amazon-ssm-agent/agent/session/
├── controlchannel/controlchannel.go            # 控制通道（Agent与MGS）
├── datachannel/datachannel.go                  # 数据通道实现
├── communicator/websocketchannel.go            # WebSocket 通道
├── contracts/agentmessage.go                   # 消息结构定义
├── crypto/blockcipher.go                       # 加密实现
└── service/service.go                          # MGS 服务调用
```

### 1.5 完整建连流程时序图

```
┌─────────┐     ┌─────────┐     ┌─────────┐     ┌─────────┐     ┌─────────┐
│  User   │     │ aws-cli │     │   SSM   │     │   MGS   │     │  Agent  │
└────┬────┘     └────┬────┘     └────┬────┘     └────┬────┘     └────┬────┘
     │               │               │               │               │
     │ 1. aws ssm    │               │               │               │
     │ start-session │               │               │               │
     │──────────────>│               │               │               │
     │               │               │               │               │
     │               │ 2. StartSession API           │               │
     │               │──────────────>│               │               │
     │               │               │               │               │
     │               │ 3. Response   │               │               │
     │               │ (SessionId,   │               │               │
     │               │  StreamUrl,   │               │               │
     │               │  TokenValue)  │               │               │
     │               │<──────────────│               │               │
     │               │               │               │               │
┌────┴────┐     ┌────┴────┐                                     ┌────┴────┐
│  User   │     │ Plugin  │                                     │  Agent  │
└────┬────┘     └────┬────┘                                     └────┬────┘
     │               │                                               │
     │               │ 4. subprocess 启动 plugin                      │
     │               │ 传递: SessionId, StreamUrl, TokenValue         │
     │               │               │               │               │
     │               │               │               │   (Agent 已通过 │
     │               │               │               │   ControlChannel│
     │               │               │               │   连接到 MGS)   │
     │               │               │               │               │
     │               │ 5. WebSocket Connect          │               │
     │               │ (带 V4 签名或预签名 URL)        │               │
     │               │──────────────────────────────>│               │
     │               │               │               │               │
     │               │ 6. OpenDataChannel            │               │
     │               │ (发送 TokenValue)              │               │
     │               │──────────────────────────────>│               │
     │               │               │               │               │
     │               │               │               │ 7. MGS 通知    │
     │               │               │               │    Agent 新会话│
     │               │               │               │──────────────>│
     │               │               │               │               │
     │               │               │               │ 8. Agent 建立  │
     │               │               │               │    DataChannel │
     │               │               │               │<──────────────│
     │               │               │               │               │
     │               │ 9. HandshakeRequest (Agent -> Plugin)         │
     │               │<─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─│
     │               │               │               │               │
     │               │ 10. HandshakeResponse (Plugin -> Agent)       │
     │               │─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─>│
     │               │               │               │               │
     │               │ 11. EncryptionChallenge (可选，双向)            │
     │               │<─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─│
     │               │─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─>│
     │               │               │               │               │
     │               │ 12. HandshakeComplete (Agent -> Plugin)       │
     │               │<─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─│
     │               │               │               │               │
     │               │═══════════════ 会话建立完成 ═══════════════════│
     │               │               │               │               │
     │ 13. 用户输入  │ 14. input_stream_data          │               │
     │──────────────>│─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─>│
     │               │               │               │               │
     │               │ 15. output_stream_data (命令输出)              │
     │ 16. 显示输出  │<─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─│
     │<──────────────│               │               │               │
     │               │               │               │               │
```

### 1.6 连接建立的核心步骤总结

| 步骤 | 发起方 | 接收方 | 描述 |
|------|--------|--------|------|
| 1 | aws-cli | SSM | 调用 StartSession API |
| 2 | SSM | aws-cli | 返回 SessionId, StreamUrl, TokenValue |
| 3 | aws-cli | plugin | subprocess 启动 plugin，传递会话参数 |
| 4 | plugin | MGS | WebSocket 连接（带签名） |
| 5 | plugin | MGS | 发送 OpenDataChannelInput（含 Token） |
| 6 | Agent | MGS | 通过 ControlChannel 接收新会话通知 |
| 7 | Agent | MGS | 建立 DataChannel WebSocket |
| 8 | Agent | Plugin | 发送 HandshakeRequest |
| 9 | Plugin | Agent | 返回 HandshakeResponse |
| 10 | 双向 | 双向 | 加密挑战交换（如果启用 KMS） |
| 11 | Agent | Plugin | 发送 HandshakeComplete |
| 12 | 双向 | 双向 | 会话数据双向传输 |

---

## 2. AWS CLI 入口分析

> 源文件: `aws-cli/awscli/customizations/sessionmanager.py`

AWS CLI 负责调用 SSM StartSession API 并启动 session-manager-plugin 子进程。

### 2.1 注册自定义命令

```python
# https://github.com/aws/aws-cli/blob/develop/awscli/customizations/sessionmanager.py#L33-35
def register_ssm_session(event_handlers):
    # 注册事件处理器，在构建 ssm 命令表时添加自定义的 start-session 命令
    event_handlers.register('building-command-table.ssm',
                            add_custom_start_session)
```

```python
# https://github.com/aws/aws-cli/blob/develop/awscli/customizations/sessionmanager.py#L38-46
def add_custom_start_session(session, command_table, **kwargs):
    # 用自定义的 StartSessionCommand 替换默认的 start-session 命令
    command_table['start-session'] = StartSessionCommand(
        name='start-session',
        parent_name='ssm',
        session=session,
        operation_model=session.get_service_model(
            'ssm').operation_model('StartSession'),  # 获取 StartSession 操作模型
        operation_caller=StartSessionCaller(session), # 使用自定义的调用器
    )
```

### 2.2 StartSessionCaller.invoke - 核心入口

```python
# https://github.com/aws/aws-cli/blob/develop/awscli/customizations/sessionmanager.py#L94-170 - 逐行注释
class StartSessionCaller(CLIOperationCaller):
    LAST_PLUGIN_VERSION_WITHOUT_ENV_VAR = "1.2.497.0"  # 支持环境变量传参的最低版本
    DEFAULT_SSM_ENV_NAME = "AWS_SSM_START_SESSION_RESPONSE"  # 环境变量名

    def invoke(self, service_name, operation_name, parameters, parsed_globals):
        # ========== 第一步: 创建 SSM 客户端并调用 StartSession API ==========
        client = self._session.create_client(
            service_name,                          # 'ssm'
            region_name=parsed_globals.region,     # 用户指定的区域
            endpoint_url=parsed_globals.endpoint_url,  # 自定义端点（可选）
            verify=parsed_globals.verify_ssl)      # SSL 验证
        
        # 调用 StartSession API，返回会话信息
        response = client.start_session(**parameters)  # parameters 包含 Target (实例ID)
        
        # ========== 第二步: 提取响应中的关键信息 ==========
        session_id = response['SessionId']         # 会话唯一标识
        region_name = client.meta.region_name      # 区域名称
        
        # 获取 profile 名称，用于 plugin 获取相同的凭证
        profile_name = parsed_globals.profile \
            if parsed_globals.profile is not None else ''
        endpoint_url = client.meta.endpoint_url    # SSM 服务端点
        ssm_env_name = self.DEFAULT_SSM_ENV_NAME

        try:
            # ========== 第三步: 构建传递给 Plugin 的会话参数 ==========
            session_parameters = {
                "SessionId": response["SessionId"],    # 会话 ID
                "TokenValue": response["TokenValue"],  # 认证 Token
                "StreamUrl": response["StreamUrl"],    # WebSocket URL (wss://...)
            }
            start_session_response = json.dumps(session_parameters)

            # ========== 第四步: 检查 Plugin 版本 ==========
            plugin_version = check_output(
                ["session-manager-plugin", "--version"], text=True
            )
            env = os.environ.copy()

            # 检查 plugin 版本是否支持通过环境变量传递响应
            # 新版本通过环境变量传递更安全（避免命令行暴露敏感信息）
            version_requirement = VersionRequirement(
                min_version=self.LAST_PLUGIN_VERSION_WITHOUT_ENV_VAR
            )
            if version_requirement.meets_requirement(plugin_version):
                # 新版本: 通过环境变量传递
                env[ssm_env_name] = start_session_response
                start_session_response = ssm_env_name  # 只传递变量名
            # 旧版本: 直接通过命令行参数传递 JSON

            # ========== 第五步: 启动 Plugin 子进程 ==========
            with ignore_user_entered_signals():  # 忽略用户信号，让 plugin 处理
                check_call([
                    "session-manager-plugin",
                    start_session_response,    # args[1]: 会话响应 JSON 或环境变量名
                    region_name,               # args[2]: 区域
                    "StartSession",            # args[3]: 操作名称
                    profile_name,              # args[4]: AWS profile
                    json.dumps(parameters),    # args[5]: 原始请求参数 (含 Target)
                    endpoint_url               # args[6]: SSM 端点 URL
                ], env=env)

            return 0

        except OSError as ex:
            if ex.errno == errno.ENOENT:
                # Plugin 未安装
                logger.debug('SessionManagerPlugin is not present', exc_info=True)
                # 终止会话，避免在 Agent 上产生僵尸会话
                client.terminate_session(SessionId=session_id)
                raise ValueError(''.join(ERROR_MESSAGE))
```

### 2.3 传递给 Plugin 的参数格式

| 参数位置 | 内容 | 示例 |
|----------|------|------|
| args[0] | 可执行文件路径 | `/usr/local/bin/session-manager-plugin` |
| args[1] | 会话响应 JSON 或环境变量名 | `{"SessionId":"...","TokenValue":"...","StreamUrl":"..."}` |
| args[2] | AWS 区域 | `us-east-1` |
| args[3] | 操作名称 | `StartSession` |
| args[4] | AWS Profile | `default` 或空字符串 |
| args[5] | 请求参数 JSON | `{"Target":"i-1234567890abcdef0"}` |
| args[6] | SSM 端点 URL | `https://ssm.us-east-1.amazonaws.com` |

### 2.4 StartSession API 响应结构

```json
{
    "SessionId": "session-1234567890abcdef0",
    "TokenValue": "AAEAAVbH...base64编码的Token...",
    "StreamUrl": "wss://ssmmessages.us-east-1.amazonaws.com/v1/data-channel/session-1234567890abcdef0?role=publish_subscribe"
}
```

- **SessionId**: 会话唯一标识符
- **TokenValue**: 用于 WebSocket 连接认证的一次性 Token
- **StreamUrl**: MGS WebSocket 端点 URL

---

## 3. Plugin 入口与 Session 初始化

> 源文件: `session-manager-plugin/src/sessionmanagerplugin-main/main.go`
> 源文件: `session-manager-plugin/src/sessionmanagerplugin/session/session.go`

### 3.1 Plugin 主入口

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/sessionmanagerplugin-main/main.go#L25-27
func main() {
    // 将命令行参数和标准输出传递给会话初始化函数
    session.ValidateInputAndStartSession(os.Args, os.Stdout)
}
```

### 3.2 ValidateInputAndStartSession - 参数解析

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/sessionmanagerplugin/session/session.go#L132-221 - 逐行注释
func ValidateInputAndStartSession(args []string, out io.Writer) {
    var (
        err                error
        session            Session
        startSessionOutput ssm.StartSessionOutput
        response           []byte
        region             string
        operationName      string
        profile            string
        ssmEndpoint        string
        target             string
    )
    
    // 初始化日志
    log := log.Logger(true, "session-manager-plugin")
    uuid.SwitchFormat(uuid.CleanHyphen)

    // ========== 参数数量检查 ==========
    if len(args) == 1 {
        // 无参数，显示安装成功信息
        fmt.Fprint(out, "\nThe Session Manager plugin was installed successfully...\n")
        return
    } else if len(args) == 2 && args[1] == "--version" {
        // 显示版本号
        fmt.Fprintf(out, "%s\n", string(version.Version))
        return
    } else if len(args) >= 2 && len(args) < LegacyArgumentLength {
        // 参数不足
        fmt.Fprintf(out, "\nUnknown operation %s...\n", string(args[1]))
        return
    } else if len(args) == LegacyArgumentLength {  // LegacyArgumentLength = 4
        // 旧版本 AWS CLI 不传递 profile，需要升级才能使用加密功能
        session.IsAwsCliUpgradeNeeded = true
    }

    // ========== 解析命令行参数 ==========
    for argsIndex := 1; argsIndex < len(args); argsIndex++ {
        switch argsIndex {
        case 1:
            // args[1]: 会话响应 JSON 或环境变量名
            if strings.HasPrefix(args[1], "AWS_SSM_START_SESSION_RESPONSE") == true {
                // 新版本: 从环境变量读取
                response = []byte(os.Getenv(args[1]))
                // 读取后立即清除环境变量（安全考虑）
                if err = os.Unsetenv(args[1]); err != nil {
                    log.Errorf("Failed to remove temporary session env parameter: %v", err)
                }
            } else {
                // 旧版本: 直接从参数读取 JSON
                response = []byte(args[1])
            }
        case 2:
            region = args[2]           // AWS 区域
        case 3:
            operationName = args[3]    // 操作名称 (StartSession)
        case 4:
            profile = args[4]          // AWS Profile
        case 5:
            // 解析请求参数，提取 Target
            startSessionRequest := make(map[string]interface{})
            json.Unmarshal([]byte(args[5]), &startSessionRequest)
            target = startSessionRequest["Target"].(string)
        case 6:
            ssmEndpoint = args[6]      // SSM 服务端点
        }
    }
    
    // 设置 SDK 的区域和 Profile
    sdkutil.SetRegionAndProfile(region, profile)
    // 生成客户端唯一 ID
    clientId := uuid.NewV4().String()

    // ========== 根据操作类型处理 ==========
    switch operationName {
    case StartSessionOperation:  // "StartSession"
        // 反序列化 StartSession API 响应
        if err = json.Unmarshal(response, &startSessionOutput); err != nil {
            log.Errorf("Cannot perform start session: %v", err)
            return
        }

        // 填充 Session 结构体
        session.SessionId = *startSessionOutput.SessionId    // 会话 ID
        session.StreamUrl = *startSessionOutput.StreamUrl    // WebSocket URL
        session.TokenValue = *startSessionOutput.TokenValue  // 认证 Token
        session.Endpoint = ssmEndpoint                       // SSM 端点
        session.ClientId = clientId                          // 客户端 ID
        session.TargetId = target                            // 目标实例 ID
        session.Region = region                              // 区域
        session.DataChannel = &datachannel.DataChannel{}     // 初始化数据通道

    default:
        fmt.Fprint(out, "Invalid Operation")
        return
    }

    // ========== 启动会话 ==========
    if err = startSession(&session, log); err != nil {
        log.Errorf("Cannot perform start session: %v", err)
        fmt.Fprintf(out, "Cannot perform start session: %v\n", err)
        return
    }
}
```

### 3.3 Session 结构体定义

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/sessionmanagerplugin/session/session.go#L74-90
type Session struct {
    DataChannel           datachannel.IDataChannel  // 数据通道接口
    SessionId             string                    // 会话 ID (MGS 分配)
    StreamUrl             string                    // WebSocket URL (wss://ssmmessages...)
    TokenValue            string                    // 认证 Token (一次性)
    IsAwsCliUpgradeNeeded bool                      // 是否需要升级 CLI
    Endpoint              string                    // SSM 服务端点
    ClientId              string                    // 客户端唯一 ID (UUID)
    TargetId              string                    // 目标实例 ID
    sdk                   *ssm.SSM                  // SSM SDK 客户端
    retryParams           retry.RepeatableExponentialRetryer  // 重试配置
    SessionType           string                    // 会话类型 (Standard_Stream/Port)
    SessionProperties     interface{}               // 会话属性
    DisplayMode           sessionutil.DisplayMode   // 显示模式
    Region                string                    // AWS 区域
    Signer                *v4.Signer                // V4 签名器 (用于 WebSocket 认证)
}
```

### 3.4 Session.Execute - 启动数据通道

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/sessionmanagerplugin/session/session.go#L224-251
func (s *Session) Execute(log log.T) (err error) {
    fmt.Fprintf(os.Stdout, "\nStarting session with SessionId: %s\n", s.SessionId)

    // 设置显示模式
    s.DisplayMode = sessionutil.NewDisplayMode(log)

    // ========== 打开数据通道 ==========
    if err = s.OpenDataChannel(log); err != nil {
        log.Errorf("Error in Opening data channel: %v", err)
        return
    }

    // 启动消息重发超时监控
    handleStreamMessageResendTimeout(s, log)

    // ========== 等待会话类型确定 ==========
    // 会话类型由握手协议或第一个数据包确定
    if !<-s.DataChannel.IsSessionTypeSet() {
        log.Errorf("unable to set SessionType for session %s", s.SessionId)
        return errors.New("unable to determine SessionType")
    } else {
        // 获取会话类型和属性
        s.SessionType = s.DataChannel.GetSessionType()
        s.SessionProperties = s.DataChannel.GetSessionProperties()
        
        // 根据会话类型设置处理器 (ShellSession/PortSession)
        if err = setSessionHandlersWithSessionType(s, log); err != nil {
            log.Errorf("Session ending with error: %v", err)
            return
        }
    }

    return
}
```

### 3.5 会话类型注册机制

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/sessionmanagerplugin/session/session.go#L46-72
// 全局会话插件注册表
var SessionRegistry = map[string]ISessionPlugin{}

// 会话插件接口
type ISessionPlugin interface {
    SetSessionHandlers(log.T) error                  // 设置处理器
    ProcessStreamMessagePayload(log.T, message.ClientMessage) (bool, error)  // 处理消息
    Initialize(log.T, *Session)                      // 初始化
    Stop()                                           // 停止
    Name() string                                    // 插件名称
}

// 注册插件
func Register(session ISessionPlugin) {
    SessionRegistry[session.Name()] = session
}

// 通过 init 函数自动注册
// https://github.com/aws/session-manager-plugin/blob/mainline/src/shellsession/shellsession.go#L49-51
func init() {
    session.Register(&ShellSession{})  // 注册 Shell 会话插件
}

// portsession/portsession.go 也有类似的注册
```

---

## 4. Agent 控制通道

> 源文件: `amazon-ssm-agent/agent/session/controlchannel/controlchannel.go`

Agent 通过控制通道与 MGS 保持长连接，接收新会话通知。

### 4.1 ControlChannel 结构体

```go
// https://github.com/aws/amazon-ssm-agent/blob/mainline/agent/session/controlchannel/controlchannel.go#L55-64
type ControlChannel struct {
    wsChannel                       communicator.IWebSocketChannel  // WebSocket 通道
    context                         context.T                       // 上下文
    ChannelId                       string                          // 通道 ID (实例 ID)
    Service                         service.Service                 // MGS 服务
    AuditLogScheduler               telemetry.IAuditLogTelemetry    // 审计日志
    TelemetryExporter               telemetryV2.ITelemetryExporter  // 遥测导出器
    channelType                     string                          // 通道类型 (subscribe)
    agentMessageIncomingMessageChan chan mgsContracts.AgentMessage  // 消息接收通道
}
```

### 4.2 控制通道初始化

```go
// https://github.com/aws/amazon-ssm-agent/blob/mainline/agent/session/controlchannel/controlchannel.go#L67-88
func (controlChannel *ControlChannel) Initialize(context context.T,
    mgsService service.Service,
    instanceId string,
    agentMessageIncomingMessageChan chan mgsContracts.AgentMessage) {

    log := context.Log()
    controlChannel.Service = mgsService
    controlChannel.ChannelId = instanceId              // 使用实例 ID 作为通道 ID
    controlChannel.channelType = mgsConfig.RoleSubscribe  // "subscribe" - 只接收消息
    controlChannel.wsChannel = &communicator.WebSocketChannel{}
    
    // 初始化审计日志调度器
    controlChannel.AuditLogScheduler = telemetry.GetAuditLogTelemetryInstance(
        context, controlChannel.wsChannel)

    controlChannel.agentMessageIncomingMessageChan = agentMessageIncomingMessageChan
    controlChannel.context = context
    log.Debugf("Initialized controlchannel for instance: %s", instanceId)
}
```

### 4.3 获取控制通道 Token

```go
// https://github.com/aws/amazon-ssm-agent/blob/mainline/agent/session/controlchannel/controlchannel.go#L269-298
func getControlChannelToken(context context.T,
    mgsService service.Service,
    instanceId string,
    requestId string,
    ableToOpenMGSConnection *uint32) (tokenValue string, err error) {

    const accessDeniedErr string = "<AccessDeniedException>"

    // 构建创建控制通道请求
    createControlChannelInput := &service.CreateControlChannelInput{
        MessageSchemaVersion: aws.String(mgsConfig.MessageSchemaVersion),
        RequestId:            aws.String(requestId),
    }
    
    log := context.Log()
    // 调用 MGS CreateControlChannel API
    createControlChannelOutput, err := mgsService.CreateControlChannel(
        log, createControlChannelInput, instanceId)
    
    if err != nil || createControlChannelOutput == nil {
        // 检查是否是访问拒绝错误
        if err != nil && strings.Contains(err.Error(), accessDeniedErr) {
            ssmconnectionchannel.SetConnectionChannel(
                context, ssmconnectionchannel.MGSFailedDueToAccessDenied)
        }
        return "", fmt.Errorf("CreateControlChannel failed with error: %s", err)
    }

    log.Debug("Successfully get controlchannel token")
    return *createControlChannelOutput.TokenValue, nil
}
```

### 4.4 打开控制通道 WebSocket

```go
// https://github.com/aws/amazon-ssm-agent/blob/mainline/agent/session/controlchannel/controlchannel.go#L211-245
func (controlChannel *ControlChannel) Open(context context.T, 
    ableToOpenMGSConnection *uint32) error {
    
    log := context.Log()
    
    // 配置 WebSocket Dialer
    controlChannelDialerInput := &websocket.Dialer{
        TLSClientConfig: network.GetDefaultTLSConfig(log, controlChannel.context.AppConfig()),
        Proxy:           http.ProxyFromEnvironment,
        WriteBufferSize: mgsConfig.ControlChannelWriteBufferSizeLimit,
    }
    
    // 打开 WebSocket 连接
    if err := controlChannel.wsChannel.Open(log, controlChannelDialerInput); err != nil {
        atomic.StoreUint32(ableToOpenMGSConnection, 0)
        return fmt.Errorf("failed to connect controlchannel with error: %s", err)
    }

    uid := uuid.New().String()

    // 发送 OpenControlChannel 消息完成握手
    openControlChannelInput := service.OpenControlChannelInput{
        MessageSchemaVersion: aws.String(mgsConfig.MessageSchemaVersion),
        RequestId:            aws.String(uid),
        TokenValue:           aws.String(controlChannel.wsChannel.GetChannelToken()),
        AgentVersion:         aws.String(version.Version),
        PlatformType:         aws.String(platform.PlatformType(log)),
    }

    jsonValue, _ := json.Marshal(openControlChannelInput)
    return controlChannel.SendMessage(log, jsonValue, websocket.TextMessage)
}
```

---

## 5. Plugin 端 DataChannel 初始化

> 源文件: `session-manager-plugin/src/datachannel/streaming.go`

### 5.1 DataChannel 结构体 (Plugin 端)

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/datachannel/streaming.go#L78-119
type DataChannel struct {
    wsChannel             communicator.IWebSocketChannel  // WebSocket 通道
    Role                  string                          // 角色 (publish_subscribe)
    ClientId              string                          // 客户端 ID
    SessionId             string                          // 会话 ID
    TargetId              string                          // 目标实例 ID
    IsAwsCliUpgradeNeeded bool                            // 是否需要升级 CLI
    
    // 序列号管理
    ExpectedSequenceNumber   int64   // 期望接收的序列号
    StreamDataSequenceNumber int64   // 发送的流数据序列号
    
    // 消息缓冲区
    OutgoingMessageBuffer ListMessageBuffer  // 待确认的出站消息
    IncomingMessageBuffer MapMessageBuffer   // 乱序到达的入站消息
    
    // RTT 计算参数
    RoundTripTime          float64        // 当前 RTT
    RoundTripTimeVariation float64        // RTT 变化量
    RetransmissionTimeout  time.Duration  // 重传超时
    
    // 加密相关
    encryption        encryption.IEncrypter  // 加密器
    encryptionEnabled bool                    // 是否启用加密

    // 会话类型
    sessionType       string         // 会话类型
    isSessionTypeSet  chan bool      // 会话类型设置信号
    sessionProperties interface{}    // 会话属性

    // 超时检测
    isStreamMessageResendTimeout chan bool  // 重发超时信号

    // 消息处理器
    outputStreamHandlers        []OutputStreamDataMessageHandler  // 输出流处理器列表
    isSessionSpecificHandlerSet bool                              // 是否设置了会话处理器

    agentVersion string  // Agent 版本
}
```

### 5.2 DataChannel 初始化 (Plugin 端)

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/datachannel/streaming.go#L165-194
func (dataChannel *DataChannel) Initialize(log log.T, clientId string, 
    sessionId string, targetId string, isAwsCliUpgradeNeeded bool) {
    
    log.Debugf("Calling Initialize Datachannel for role: %s", config.RolePublishSubscribe)

    dataChannel.Role = config.RolePublishSubscribe   // "publish_subscribe"
    dataChannel.ClientId = clientId
    dataChannel.SessionId = sessionId
    dataChannel.TargetId = targetId
    dataChannel.ExpectedSequenceNumber = 0           // 初始期望序列号
    dataChannel.StreamDataSequenceNumber = 0         // 初始发送序列号
    
    // 初始化出站消息缓冲区 (链表实现，便于删除已确认消息)
    dataChannel.OutgoingMessageBuffer = ListMessageBuffer{
        list.New(),
        config.OutgoingMessageBufferCapacity,  // 10000
        &sync.Mutex{},
    }
    
    // 初始化入站消息缓冲区 (Map实现，便于按序列号查找)
    dataChannel.IncomingMessageBuffer = MapMessageBuffer{
        make(map[int64]StreamingMessage),
        config.IncomingMessageBufferCapacity,  // 10000
        &sync.Mutex{},
    }
    
    // RTT 初始值
    dataChannel.RoundTripTime = float64(config.DefaultRoundTripTime)  // 100ms
    dataChannel.RoundTripTimeVariation = config.DefaultRoundTripTimeVariation  // 0
    dataChannel.RetransmissionTimeout = config.DefaultTransmissionTimeout  // 200ms
    
    dataChannel.wsChannel = &communicator.WebSocketChannel{}
    dataChannel.encryptionEnabled = false
    dataChannel.isSessionTypeSet = make(chan bool, 1)
    dataChannel.isStreamMessageResendTimeout = make(chan bool, 1)
    dataChannel.sessionType = ""
    dataChannel.IsAwsCliUpgradeNeeded = isAwsCliUpgradeNeeded
}
```

### 5.3 打开 DataChannel (Plugin 端)

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/datachannel/streaming.go#L230-239
func (dataChannel *DataChannel) Open(log log.T) (err error) {
    // 打开 WebSocket 连接
    if err = dataChannel.wsChannel.Open(log); err != nil {
        return fmt.Errorf("failed to open data channel with error: %v", err)
    }

    // 发送 Token 完成握手
    if err = dataChannel.FinalizeDataChannelHandshake(
        log, dataChannel.wsChannel.GetChannelToken()); err != nil {
        return fmt.Errorf("error sending token for handshake: %v", err)
    }
    return
}
```

### 5.4 FinalizeDataChannelHandshake - 发送 Token

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/datachannel/streaming.go#L202-222
func (dataChannel *DataChannel) FinalizeDataChannelHandshake(
    log log.T, tokenValue string) (err error) {
    
    uuid.SwitchFormat(uuid.CleanHyphen)
    uid := uuid.NewV4().String()

    log.Infof("Sending token through data channel %s to acknowledge connection", 
        dataChannel.wsChannel.GetStreamUrl())
    
    // 构建 OpenDataChannel 请求
    openDataChannelInput := service.OpenDataChannelInput{
        MessageSchemaVersion: aws.String(config.MessageSchemaVersion),  // "1.0"
        RequestId:            aws.String(uid),
        TokenValue:           aws.String(tokenValue),    // 从 StartSession API 获取的 Token
        ClientId:             aws.String(dataChannel.ClientId),
        ClientVersion:        aws.String(version.Version),
    }

    openDataChannelInputBytes, _ := json.Marshal(openDataChannelInput)
    
    // 通过 WebSocket 发送 JSON 消息
    return dataChannel.SendMessage(log, openDataChannelInputBytes, websocket.TextMessage)
}
```

---

## 6. Agent 端数据通道

> 源文件: `amazon-ssm-agent/agent/session/datachannel/datachannel.go`

### 6.1 DataChannel 结构体 (Agent 端)

```go
// https://github.com/aws/amazon-ssm-agent/blob/mainline/agent/session/datachannel/datachannel.go#L84-123
type DataChannel struct {
    wsChannel  communicator.IWebSocketChannel  // WebSocket 通道
    context    context.T
    Service    service.Service
    ChannelId  string   // SessionId
    ClientId   string   // 客户端 ID
    InstanceId string   // 实例 ID
    Role       string   // 角色
    Pause      bool     // 暂停标志
    
    // 序列号管理
    ExpectedSequenceNumber        int64        // 期望接收序列号
    StreamDataSequenceNumber      int64        // 发送序列号
    StreamDataSequenceNumberMutex *sync.Mutex  // 序列号互斥锁
    
    // 消息缓冲区
    OutgoingMessageBuffer ListMessageBuffer
    IncomingMessageBuffer MapMessageBuffer
    
    // RTT 参数
    RoundTripTime          float64
    RoundTripTimeVariation float64
    RetransmissionTimeout  time.Duration
    
    // 取消标志 - 用于通知插件取消
    cancelFlag task.CancelFlag
    
    // 消息处理器
    inputStreamMessageHandler func(log.T, mgsContracts.AgentMessage) error
    
    // 握手状态
    handshake Handshake
    
    // 加密
    blockCipher       crypto.IBlockCipher
    encryptionEnabled bool
    separateOutputPayload bool
}
```

### 6.2 Agent 端 DataChannel 初始化

```go
// https://github.com/aws/amazon-ssm-agent/blob/mainline/agent/session/datachannel/datachannel.go#L227-271
func (dataChannel *DataChannel) Initialize(context context.T,
    mgsService service.Service,
    sessionId string,
    clientId string,
    instanceId string,
    role string,
    cancelFlag task.CancelFlag,
    inputStreamMessageHandler InputStreamMessageHandler) {

    dataChannel.context = context
    dataChannel.Service = mgsService
    dataChannel.ChannelId = sessionId
    dataChannel.ClientId = clientId
    dataChannel.InstanceId = instanceId
    dataChannel.Role = role
    dataChannel.Pause = false
    dataChannel.ExpectedSequenceNumber = 0
    dataChannel.StreamDataSequenceNumber = 0
    dataChannel.StreamDataSequenceNumberMutex = &sync.Mutex{}
    
    // 初始化缓冲区
    dataChannel.OutgoingMessageBuffer = ListMessageBuffer{
        list.New(),
        mgsConfig.OutgoingMessageBufferCapacity,
        &sync.Mutex{},
    }
    dataChannel.IncomingMessageBuffer = MapMessageBuffer{
        make(map[int64]StreamingMessage),
        mgsConfig.IncomingMessageBufferCapacity,
        &sync.Mutex{},
    }
    
    // RTT 初始化
    dataChannel.RoundTripTime = float64(mgsConfig.DefaultRoundTripTime)
    dataChannel.RoundTripTimeVariation = mgsConfig.DefaultRoundTripTimeVariation
    dataChannel.RetransmissionTimeout = mgsConfig.DefaultTransmissionTimeout
    
    dataChannel.wsChannel = &communicator.WebSocketChannel{}
    dataChannel.cancelFlag = cancelFlag
    dataChannel.inputStreamMessageHandler = inputStreamMessageHandler
    
    // 初始化握手状态
    dataChannel.handshake = Handshake{
        responseChan:            make(chan bool),
        encryptionConfirmedChan: make(chan bool),
        error:                   nil,
        complete:                false,
        skipped:                 false,
    }
}
```

### 6.3 Agent 端打开 DataChannel

```go
// https://github.com/aws/amazon-ssm-agent/blob/mainline/agent/session/datachannel/datachannel.go#L346-368
func (dataChannel *DataChannel) Open(log log.T) error {
    // 打开 WebSocket 连接
    if err := dataChannel.wsChannel.Open(log, nil); err != nil {
        return fmt.Errorf("failed to connect data channel with error: %s", err)
    }

    // 发送 OpenDataChannel 消息
    uid := uuid.New().String()
    openDataChannelInput := service.OpenDataChannelInput{
        MessageSchemaVersion: aws.String(mgsConfig.MessageSchemaVersion),
        RequestId:            aws.String(uid),
        TokenValue:           aws.String(dataChannel.wsChannel.GetChannelToken()),
        ClientInstanceId:     aws.String(dataChannel.InstanceId),  // Agent 端特有
        ClientId:             aws.String(dataChannel.ClientId),
    }
    
    jsonValue, _ := json.Marshal(openDataChannelInput)
    return dataChannel.SendMessage(log, jsonValue, websocket.TextMessage)
}
```

---

## 7. WebSocket 连接建立详解

> 源文件: `session-manager-plugin/src/communicator/websocketchannel.go`
> 源文件: `session-manager-plugin/src/websocketutil/websocketutil.go`

### 7.1 WebSocketChannel 初始化 (Plugin 端)

```go
// (Plugin) https://github.com/aws/session-manager-plugin/blob/mainline/src/communicator/websocketchannel.go#L85-90
func (webSocketChannel *WebSocketChannel) Initialize(log log.T, 
    channelUrl string, channelToken string, region string, signer *v4.Signer) {
    
    webSocketChannel.ChannelToken = channelToken  // 认证 Token
    webSocketChannel.Url = channelUrl             // WebSocket URL (wss://ssmmessages...)
    webSocketChannel.Region = region              // 区域
    webSocketChannel.Signer = signer              // V4 签名器 (可能为 nil)
}
```

### 7.2 打开 WebSocket 连接

```go
// (Plugin) https://github.com/aws/session-manager-plugin/blob/mainline/src/communicator/websocketchannel.go#L187-251
func (webSocketChannel *WebSocketChannel) Open(log log.T) error {
    // 初始化写互斥锁
    webSocketChannel.writeLock = &sync.Mutex{}
    
    // ========== 检查 URL 是否已预签名 ==========
    presigned, err := IsPresignedURL(webSocketChannel.Url)
    if err != nil {
        return err
    }

    // ========== 获取认证头 ==========
    var header http.Header
    if !presigned {
        // URL 未预签名，需要生成 V4 签名头
        header, err = webSocketChannel.getV4SignatureHeader(log, webSocketChannel.Url)
        if err != nil {
            log.Errorf("Failed to get the v4 signature, %v", err)
        }
    }
    // 如果 URL 已预签名，则不需要额外认证头

    // ========== 建立 WebSocket 连接 ==========
    ws, err := websocketutil.NewWebsocketUtil(log, nil).OpenConnection(
        webSocketChannel.Url, header)
    if err != nil {
        log.Errorf("Failed to open WebSocket connection: %v", err)
        return err
    }
    webSocketChannel.Connection = ws
    webSocketChannel.IsOpen = true
    
    // ========== 启动心跳 Ping ==========
    webSocketChannel.StartPings(log, config.PingTimeInterval)  // 每 5 分钟

    // ========== 启动消息接收 goroutine ==========
    go func() {
        defer func() {
            if msg := recover(); msg != nil {
                log.Errorf("WebsocketChannel listener run panic: %v", msg)
            }
        }()

        retryCount := 0
        for {
            if webSocketChannel.IsOpen == false {
                log.Debugf("Ending the channel listening routine since the channel is closed")
                break
            }

            // 读取消息
            messageType, rawMessage, err := webSocketChannel.Connection.ReadMessage()
            if err != nil {
                retryCount++
                if retryCount >= config.RetryAttempt {  // 5 次
                    log.Errorf("Reach the retry limit %v for receive messages.", config.RetryAttempt)
                    webSocketChannel.OnError(err)  // 触发错误处理/重连
                    break
                }
                continue
            } else if messageType != websocket.TextMessage && 
                      messageType != websocket.BinaryMessage {
                log.Errorf("Invalid message type: %v", messageType)
            } else {
                retryCount = 0
                webSocketChannel.OnMessage(rawMessage)  // 调用消息处理器
            }
        }
    }()
    return nil
}
```

### 7.3 V4 签名头生成

```go
// (Plugin) https://github.com/aws/session-manager-plugin/blob/mainline/src/communicator/websocketchannel.go#L132-142
func (webSocketChannel *WebSocketChannel) getV4SignatureHeader(
    log log.T, Url string) (http.Header, error) {
    
    // 创建 HTTP 请求对象
    request, err := http.NewRequest("GET", Url, nil)

    // 如果有签名器，对请求进行 V4 签名
    if webSocketChannel.Signer != nil {
        _, err = webSocketChannel.Signer.Sign(
            request,
            nil,                           // body
            config.ServiceName,            // "ssmmessages"
            webSocketChannel.Region,       // 区域
            time.Now(),                    // 签名时间
        )
        if err != nil {
            log.Errorf("Failed to sign websocket, %v", err)
        }
    }
    return request.Header, err  // 返回包含签名的 Header
}
```

### 7.4 预签名 URL 检测

```go
// (Plugin) https://github.com/aws/session-manager-plugin/blob/mainline/src/communicator/websocketchannel.go#L145-170
func IsPresignedURL(rawURL string) (bool, error) {
    parsedURL, err := url.Parse(rawURL)
    if err != nil {
        return false, err
    }

    queryParams := parsedURL.Query()

    // 检查是否包含预签名参数
    presignedURLParams := []string{
        "X-Amz-Algorithm",      // 签名算法
        "X-Amz-Credential",     // 凭证
        "X-Amz-Date",           // 日期
        "X-Amz-Expires",        // 过期时间
        "X-Amz-SignedHeaders",  // 签名头
        "X-Amz-Signature",      // 签名
        "X-Amz-Security-Token", // 临时凭证 Token
    }

    for _, param := range presignedURLParams {
        if _, exists := queryParams[param]; exists {
            return true, nil  // 找到任一预签名参数，说明 URL 已预签名
        }
    }

    return false, nil
}
```

### 7.5 WebSocket Dial 连接

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/websocketutil/websocketutil.go#L58-79
func (u *WebsocketUtil) OpenConnection(url string, header http.Header) (
    *websocket.Conn, error) {

    if header != nil {
        u.log.Infof("Opening websocket connection to: %s with auth header", url)
    } else {
        u.log.Infof("Opening websocket connection to: %s", url)
    }

    // 使用 gorilla/websocket 建立连接
    conn, resp, err := u.dialer.Dial(url, header)
    if err != nil {
        if resp != nil {
            u.log.Errorf("Failed to dial websocket, status: %s, err: %s", 
                resp.Status, err)
        } else {
            u.log.Errorf("Failed to dial websocket: %s", err)
        }
        return nil, err
    }

    u.log.Infof("Successfully opened websocket connection to: ", url)
    return conn, err
}
```

---

## 8. 握手协议详解

> Agent 发起握手 -> Plugin 响应 -> Agent 完成握手

### 8.1 Agent 发起 HandshakeRequest

```go
// https://github.com/aws/amazon-ssm-agent/blob/mainline/agent/datachannel.go:956-1081
func (dataChannel *DataChannel) PerformHandshake(log log.T,
    kmsKeyId string,
    encryptionEnabled bool,
    sessionTypeRequest mgsContracts.SessionTypeRequest) (err error) {

    // 初始化加密器
    if encryptionEnabled {
        if dataChannel.blockCipher, err = newBlockCipher(
            dataChannel.context, kmsKeyId); err != nil {
            return fmt.Errorf("Initializing BlockCipher failed: %s", err)
        }
    }

    dataChannel.handshake.handshakeStartTime = time.Now()
    dataChannel.encryptionEnabled = encryptionEnabled

    // 设置握手超时
    handshakeTimeout := time.Duration(
        dataChannel.context.AppConfig().Ssm.SessionHandshakeTimeoutSeconds) * time.Second

    log.Info(fmt.Sprintf("Initiating Handshake with HandshakeTimeout: %v", handshakeTimeout))
    
    // 构建 HandshakeRequest
    handshakeRequestPayload := dataChannel.buildHandshakeRequestPayload(
        log, dataChannel.encryptionEnabled, sessionTypeRequest)
    
    // 发送 HandshakeRequest
    if err := dataChannel.sendHandshakeRequest(log, handshakeRequestPayload); err != nil {
        return err
    }

    // 等待 HandshakeResponse
    select {
    case <-dataChannel.handshake.responseChan:
        if dataChannel.handshake.error != nil {
            return dataChannel.handshake.error
        }
    case <-time.After(handshakeTimeout):
        return errors.New("Handshake timed out. Please ensure latest plugin version.")
    }

    // 如果启用加密，发送加密挑战
    if dataChannel.encryptionEnabled {
        dataChannel.sendEncryptionChallenge(log)
        select {
        case <-dataChannel.handshake.encryptionConfirmedChan:
            if dataChannel.handshake.error != nil {
                return dataChannel.handshake.error
            }
            log.Info("Encryption challenge confirmed.")
        case <-time.After(handshakeTimeout):
            return errors.New("Timed out waiting for encryption challenge.")
        }
    }

    // 发送 HandshakeComplete
    dataChannel.handshake.handshakeEndTime = time.Now()
    handshakeCompletePayload := dataChannel.buildHandshakeCompletePayload(log)
    if err := dataChannel.sendHandshakeComplete(log, handshakeCompletePayload); err != nil {
        return err
    }
    dataChannel.handshake.complete = true
    log.Info("Handshake successfully completed.")
    return
}
```

### 8.2 HandshakeRequest 消息结构

```go
// https://github.com/aws/amazon-ssm-agent/blob/mainline/agent/contracts/handshake.go
type HandshakeRequestPayload struct {
    AgentVersion           string                  `json:"AgentVersion"`
    RequestedClientActions []RequestedClientAction `json:"RequestedClientActions"`
}

type RequestedClientAction struct {
    ActionType       ActionType      `json:"ActionType"`      // "SessionType" 或 "KMSEncryption"
    ActionParameters interface{}     `json:"ActionParameters"`
}

// 构建 HandshakeRequest
// https://github.com/aws/amazon-ssm-agent/blob/mainline/agent/datachannel.go:1024-1046
func (dataChannel *DataChannel) buildHandshakeRequestPayload(log log.T,
    encryptionRequested bool,
    request mgsContracts.SessionTypeRequest) mgsContracts.HandshakeRequestPayload {

    handshakeRequest := mgsContracts.HandshakeRequestPayload{}
    handshakeRequest.AgentVersion = version.Version
    
    // 添加 SessionType Action
    handshakeRequest.RequestedClientActions = []mgsContracts.RequestedClientAction{
        {
            ActionType:       mgsContracts.SessionType,  // 告知 Plugin 会话类型
            ActionParameters: request,
        }}
    
    // 如果需要加密，添加 KMSEncryption Action
    if encryptionRequested {
        handshakeRequest.RequestedClientActions = append(
            handshakeRequest.RequestedClientActions,
            mgsContracts.RequestedClientAction{
                ActionType: mgsContracts.KMSEncryption,
                ActionParameters: mgsContracts.KMSEncryptionRequest{
                    KMSKeyID:  dataChannel.blockCipher.GetKMSKeyId(),
                    Challenge: dataChannel.blockCipher.GetRandomChallenge(),
                }})
    }

    return handshakeRequest
}
```

### 8.3 Plugin 处理 HandshakeRequest

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/datachannel/streaming.go#L435-492
func (dataChannel *DataChannel) handleHandshakeRequest(
    log log.T, clientMessage message.ClientMessage) error {

    // 反序列化 HandshakeRequest
    handshakeRequest, err := clientMessage.DeserializeHandshakeRequest(log)
    if err != nil {
        log.Errorf("Deserialize Handshake Request failed: %s", err)
        return err
    }

    dataChannel.agentVersion = handshakeRequest.AgentVersion

    var errorList []error
    var handshakeResponse message.HandshakeResponsePayload
    handshakeResponse.ClientVersion = version.Version
    handshakeResponse.ProcessedClientActions = []message.ProcessedClientAction{}
    
    // 处理每个 RequestedAction
    for _, action := range handshakeRequest.RequestedClientActions {
        processedAction := message.ProcessedClientAction{}
        
        switch action.ActionType {
        case message.KMSEncryption:
            processedAction.ActionType = action.ActionType
            // 处理 KMS 加密请求
            err := dataChannel.ProcessKMSEncryptionHandshakeAction(
                log, action.ActionParameters)
            if err != nil {
                processedAction.ActionStatus = message.Failed
                processedAction.Error = fmt.Sprintf("Failed: %s", err)
                errorList = append(errorList, err)
            } else {
                processedAction.ActionStatus = message.Success
                processedAction.ActionResult = message.KMSEncryptionResponse{
                    KMSCipherTextKey: dataChannel.encryption.GetEncryptedDataKey(),
                }
                dataChannel.encryptionEnabled = true
            }
            
        case message.SessionType:
            processedAction.ActionType = action.ActionType
            // 处理会话类型
            err := dataChannel.ProcessSessionTypeHandshakeAction(action.ActionParameters)
            if err != nil {
                processedAction.ActionStatus = message.Failed
            } else {
                processedAction.ActionStatus = message.Success
            }
            
        default:
            processedAction.ActionStatus = message.Unsupported
        }
        handshakeResponse.ProcessedClientActions = append(
            handshakeResponse.ProcessedClientActions, processedAction)
    }
    
    // 发送 HandshakeResponse
    return dataChannel.sendHandshakeResponse(log, handshakeResponse)
}
```

### 8.4 Plugin 处理 HandshakeComplete

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/datachannel/streaming.go#L496-519
func (dataChannel *DataChannel) handleHandshakeComplete(
    log log.T, clientMessage message.ClientMessage) error {
    
    var err error
    var handshakeComplete message.HandshakeCompletePayload
    handshakeComplete, err = clientMessage.DeserializeHandshakeComplete(log)
    if err != nil {
        return err
    }

    // SessionType 在处理 HandshakeRequest 时已设置
    if dataChannel.sessionType != "" {
        dataChannel.isSessionTypeSet <- true   // 通知会话类型已确定
    } else {
        dataChannel.isSessionTypeSet <- false
    }

    log.Debugf("Handshake Complete. Time to complete: %s seconds",
        handshakeComplete.HandshakeTimeToComplete.Seconds())

    // 显示客户信息（如加密提示）
    if handshakeComplete.CustomerMessage != "" {
        fmt.Fprintln(os.Stdout, handshakeComplete.CustomerMessage)
    }

    return err
}
```

---

## 9. KMS 加密协商

### 9.1 Plugin 处理 KMS 加密请求

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/datachannel/streaming.go#L851-869
func (dataChannel *DataChannel) ProcessKMSEncryptionHandshakeAction(
    log log.T, actionParams json.RawMessage) (err error) {

    // 检查 CLI 版本是否支持加密
    if dataChannel.IsAwsCliUpgradeNeeded {
        return errors.New("CLI version does not support encryption. Please upgrade.")
    }
    
    // 解析 KMS 加密请求
    kmsEncRequest := message.KMSEncryptionRequest{}
    json.Unmarshal(actionParams, &kmsEncRequest)
    kmsKeyId := kmsEncRequest.KMSKeyID

    // 创建 KMS 服务客户端
    kmsService, err := encryption.NewKMSService(log)
    if err != nil {
        return fmt.Errorf("error while creating new KMS service, %v", err)
    }

    // 创建加密上下文
    encryptionContext := map[string]*string{
        "aws:ssm:SessionId": &dataChannel.SessionId,
        "aws:ssm:TargetId":  &dataChannel.TargetId,
    }
    
    // 初始化加密器
    dataChannel.encryption, err = newEncrypter(
        log, kmsKeyId, encryptionContext, kmsService)
    return
}
```

### 9.2 加密器初始化和密钥生成

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/encryption/encrypter.go#L50-68
var NewEncrypter = func(log log.T, kmsKeyId string, 
    context map[string]*string, KMSService kmsiface.KMSAPI) (*Encrypter, error) {
    
    encrypter := Encrypter{kmsKeyId: kmsKeyId, KMSService: KMSService}
    err := encrypter.generateEncryptionKey(log, kmsKeyId, context)
    return &encrypter, err
}

// 生成加密密钥
func (encrypter *Encrypter) generateEncryptionKey(
    log log.T, kmsKeyId string, context map[string]*string) error {
    
    // 调用 KMS GenerateDataKey API
    cipherTextKey, plainTextKey, err := KMSGenerateDataKey(
        kmsKeyId, encrypter.KMSService, context)
    if err != nil {
        log.Errorf("Error generating data key from KMS: %s,", err)
        return err
    }
    
    // 将明文密钥分为两半：解密密钥和加密密钥
    keySize := len(plainTextKey) / 2
    encrypter.decryptionKey = plainTextKey[:keySize]   // 前半部分用于解密
    encrypter.encryptionKey = plainTextKey[keySize:]   // 后半部分用于加密
    encrypter.cipherTextKey = cipherTextKey            // 密文密钥（返回给 Agent）
    return nil
}
```

### 9.3 AES-GCM 加解密实现

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/encryption/encrypter.go#L95-137
// 加密
func (encrypter *Encrypter) Encrypt(log log.T, plainText []byte) (cipherText []byte, err error) {
    // 创建 AES-GCM 加密器
    var aesgcm cipher.AEAD
    if aesgcm, err = getAEAD(encrypter.encryptionKey); err != nil {
        return
    }

    // 生成随机 nonce (12 字节)
    cipherText = make([]byte, nonceSize+len(plainText))
    nonce := make([]byte, nonceSize)  // nonceSize = 12
    if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
        return
    }

    // 加密
    cipherTextWithoutNonce := aesgcm.Seal(nil, nonce, plainText, nil)

    // 将 nonce 附加到密文前面
    cipherText = append(cipherText[:nonceSize], nonce...)
    cipherText = append(cipherText[nonceSize:], cipherTextWithoutNonce...)
    return cipherText, nil
}

// 解密
func (encrypter *Encrypter) Decrypt(log log.T, cipherText []byte) (plainText []byte, err error) {
    var aesgcm cipher.AEAD
    if aesgcm, err = getAEAD(encrypter.decryptionKey); err != nil {
        return
    }

    // 从密文中提取 nonce
    nonce := cipherText[:nonceSize]
    cipherTextWithoutNonce := cipherText[nonceSize:]

    // 解密
    if plainText, err = aesgcm.Open(nil, nonce, cipherTextWithoutNonce, nil); err != nil {
        return
    }
    return plainText, nil
}
```

### 9.4 加密挑战流程

```
Agent                                      Plugin
  │                                          │
  │  HandshakeRequest (KMSEncryption)        │
  │  - KMSKeyID                              │
  │  - Challenge (Agent 生成的随机挑战)        │
  │ ---------------------------------------->|
  │                                          │
  │                                          | 调用 KMS GenerateDataKey
  │                                          | 获取加密密钥
  │                                          │
  │  HandshakeResponse                       │
  │  - KMSCipherTextKey (密文密钥)            │
  │ <----------------------------------------|
  │                                          │
  | 用 KMS Decrypt 解密得到明文密钥              │
  │                                          │
  │  EncryptionChallengeRequest              │
  │  - Challenge (加密后的随机数据)              │
  │ ---------------------------------------->|
  │                                          │
  │                                          | 解密 -> 重新加密
  │                                          │
  │  EncryptionChallengeResponse             │
  │  - Challenge (重新加密后的数据)              │
  │ <----------------------------------------|
  │                                          │
  | 验证解密结果是否匹配原始数据                  │
  │                                          │
  │  HandshakeComplete                       │
  │ ---------------------------------------->|
```

---

## 10. 消息协议和序列化

### 10.1 ClientMessage 二进制格式 (Plugin 端)

```
消息布局 (字节偏移):
+-------+------------------+-----+------+------+-------+
| HL(4) | MessageType(32)  |Ver(4)|CD(8) |Seq(8)|Flags(8)|
+-------+------------------+-----+------+------+-------+
|        MessageId(16)           |     Digest(32)       |
+--------------------------------+----------------------+
|   PayloadType(4)  | PayloadLength(4) |   Payload...   |
+-------------------+------------------+----------------+

字段说明:
- HL (4 字节): Header Length - 头部长度
- MessageType (32 字节): 消息类型
  - "input_stream_data"  - 输入流数据
  - "output_stream_data" - 输出流数据
  - "acknowledge"        - 确认消息
  - "channel_closed"     - 通道关闭
- Ver (4 字节): Schema Version
- CD (8 字节): Created Date (epoch millis)
- Seq (8 字节): Sequence Number - 序列号
- Flags (8 字节): 控制标志
  - Bit 0: SYN - 流开始
  - Bit 1: FIN - 流结束
- MessageId (16 字节): UUID
- Digest (32 字节): Payload 的 SHA-256 哈希
- PayloadType (4 字节): 负载类型
- PayloadLength (4 字节): 负载长度
- Payload: 变长负载数据
```

### 10.2 PayloadType 定义

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/message/clientmessage.go#L73-88
type PayloadType uint32

const (
    Output                       PayloadType = 1   // 输出数据
    Error                        PayloadType = 2   // 错误
    Size                         PayloadType = 3   // 终端大小
    Parameter                    PayloadType = 4   // 参数
    HandshakeRequestPayloadType  PayloadType = 5   // 握手请求
    HandshakeResponsePayloadType PayloadType = 6   // 握手响应
    HandshakeCompletePayloadType PayloadType = 7   // 握手完成
    EncChallengeRequest          PayloadType = 8   // 加密挑战请求
    EncChallengeResponse         PayloadType = 9   // 加密挑战响应
    Flag                         PayloadType = 10  // 标志
    StdErr                       PayloadType = 11  // 标准错误
    ExitCode                     PayloadType = 12  // 退出码
)
```

### 10.3 消息序列化

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/message/messageparser.go#L254-339
func (clientMessage *ClientMessage) SerializeClientMessage(log log.T) (result []byte, err error) {
    payloadLength := uint32(len(clientMessage.Payload))
    headerLength := uint32(ClientMessage_PayloadLengthOffset)
    clientMessage.PayloadLength = payloadLength

    totalMessageLength := headerLength + ClientMessage_PayloadLengthLength + payloadLength
    result = make([]byte, totalMessageLength)

    // 写入各字段 (Big Endian)
    putUInteger(log, result, ClientMessage_HLOffset, headerLength)  // 头部长度
    putString(log, result, ClientMessage_MessageTypeOffset, 
        ClientMessage_MessageTypeOffset + ClientMessage_MessageTypeLength - 1,
        clientMessage.MessageType)  // 消息类型
    putUInteger(log, result, ClientMessage_SchemaVersionOffset, 
        clientMessage.SchemaVersion)  // 版本
    putULong(log, result, ClientMessage_CreatedDateOffset, 
        clientMessage.CreatedDate)  // 创建时间
    putLong(log, result, ClientMessage_SequenceNumberOffset, 
        clientMessage.SequenceNumber)  // 序列号
    putULong(log, result, ClientMessage_FlagsOffset, clientMessage.Flags)  // 标志
    putUuid(log, result, ClientMessage_MessageIdOffset, clientMessage.MessageId)  // 消息ID

    // 计算并写入 Payload 的 SHA-256 哈希
    hasher := sha256.New()
    hasher.Write(clientMessage.Payload)
    putBytes(log, result, ClientMessage_PayloadDigestOffset,
        ClientMessage_PayloadDigestOffset + ClientMessage_PayloadDigestLength - 1,
        hasher.Sum(nil))

    putUInteger(log, result, ClientMessage_PayloadTypeOffset, 
        clientMessage.PayloadType)  // 负载类型
    putUInteger(log, result, ClientMessage_PayloadLengthOffset, 
        clientMessage.PayloadLength)  // 负载长度
    putBytes(log, result, ClientMessage_PayloadOffset,
        ClientMessage_PayloadOffset + int(payloadLength) - 1,
        clientMessage.Payload)  // 负载数据

    return result, nil
}
```

### 10.4 消息反序列化

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/message/messageparser.go#L36-89
func (clientMessage *ClientMessage) DeserializeClientMessage(
    log log.T, input []byte) (err error) {
    
    // 读取各字段 (Big Endian)
    clientMessage.MessageType, _ = getString(log, input, 
        ClientMessage_MessageTypeOffset, ClientMessage_MessageTypeLength)
    clientMessage.SchemaVersion, _ = getUInteger(log, input, 
        ClientMessage_SchemaVersionOffset)
    clientMessage.CreatedDate, _ = getULong(log, input, 
        ClientMessage_CreatedDateOffset)
    clientMessage.SequenceNumber, _ = getLong(log, input, 
        ClientMessage_SequenceNumberOffset)
    clientMessage.Flags, _ = getULong(log, input, ClientMessage_FlagsOffset)
    clientMessage.MessageId, _ = getUuid(log, input, ClientMessage_MessageIdOffset)
    clientMessage.PayloadDigest, _ = getBytes(log, input, 
        ClientMessage_PayloadDigestOffset, ClientMessage_PayloadDigestLength)
    clientMessage.PayloadType, _ = getUInteger(log, input, 
        ClientMessage_PayloadTypeOffset)
    clientMessage.PayloadLength, _ = getUInteger(log, input, 
        ClientMessage_PayloadLengthOffset)

    headerLength, _ := getUInteger(log, input, ClientMessage_HLOffset)
    clientMessage.HeaderLength = headerLength
    
    // 提取 Payload
    clientMessage.Payload = input[headerLength+ClientMessage_PayloadLengthLength:]

    return err
}
```

---

## 11. 消息确认与重传

### 11.1 ACK 消息结构

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/message/clientmessage.go#L48-53
type AcknowledgeContent struct {
    MessageType         string `json:"AcknowledgedMessageType"`      // 被确认的消息类型
    MessageId           string `json:"AcknowledgedMessageId"`        // 被确认的消息 ID
    SequenceNumber      int64  `json:"AcknowledgedMessageSequenceNumber"`  // 被确认的序列号
    IsSequentialMessage bool   `json:"IsSequentialMessage"`          // 是否是顺序消息
}
```

### 11.2 发送 ACK 消息

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/datachannel/streaming.go#L383-402
func (dataChannel *DataChannel) SendAcknowledgeMessage(
    log log.T, streamDataMessage message.ClientMessage) (err error) {
    
    // 构建 ACK 内容
    dataStreamAcknowledgeContent := message.AcknowledgeContent{
        MessageType:         streamDataMessage.MessageType,
        MessageId:           streamDataMessage.MessageId.String(),
        SequenceNumber:      streamDataMessage.SequenceNumber,
        IsSequentialMessage: true,
    }

    // 序列化并发送
    msg, err := message.SerializeClientMessageWithAcknowledgeContent(
        log, dataStreamAcknowledgeContent)
    if err != nil {
        log.Errorf("Cannot serialize Acknowledge message err: %v", err)
        return
    }

    if err = SendMessageCall(log, dataChannel, msg, websocket.BinaryMessage); err != nil {
        log.Errorf("Error sending acknowledge message %v", err)
        return
    }
    return
}
```

### 11.3 处理收到的 ACK

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/datachannel/streaming.go#L366-380
func (dataChannel *DataChannel) ProcessAcknowledgedMessage(
    log log.T, acknowledgeMessageContent message.AcknowledgeContent) error {
    
    acknowledgeSequenceNumber := acknowledgeMessageContent.SequenceNumber
    
    // 在出站缓冲区中查找对应的消息
    for streamMessageElement := dataChannel.OutgoingMessageBuffer.Messages.Front(); 
        streamMessageElement != nil; 
        streamMessageElement = streamMessageElement.Next() {
        
        streamMessage := streamMessageElement.Value.(StreamingMessage)
        if streamMessage.SequenceNumber == acknowledgeSequenceNumber {
            // 计算 RTT 并更新重传超时
            dataChannel.CalculateRetransmissionTimeout(log, streamMessage)
            // 从缓冲区删除已确认的消息
            dataChannel.RemoveDataFromOutgoingMessageBuffer(streamMessageElement)
            break
        }
    }
    return nil
}
```

### 11.4 RTT 计算和重传超时

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/datachannel/streaming.go#L831-847
func (dataChannel *DataChannel) CalculateRetransmissionTimeout(
    log log.T, streamingMessage StreamingMessage) {
    
    // 获取新的 RTT
    newRoundTripTime := float64(GetRoundTripTime(streamingMessage))

    // RTT 变化量计算 (RFC 6298 算法)
    // RTTVAR = (1 - beta) * RTTVAR + beta * |SRTT - R|
    dataChannel.RoundTripTimeVariation = 
        ((1 - config.RTTVConstant) * dataChannel.RoundTripTimeVariation) +
        (config.RTTVConstant * math.Abs(dataChannel.RoundTripTime - newRoundTripTime))

    // RTT 平滑值计算
    // SRTT = (1 - alpha) * SRTT + alpha * R
    dataChannel.RoundTripTime = 
        ((1 - config.RTTConstant) * dataChannel.RoundTripTime) +
        (config.RTTConstant * newRoundTripTime)

    // 计算重传超时
    // RTO = SRTT + max(G, 4*RTTVAR)
    dataChannel.RetransmissionTimeout = time.Duration(
        dataChannel.RoundTripTime +
        math.Max(float64(config.ClockGranularity), 
                 float64(4*dataChannel.RoundTripTimeVariation)))

    // 确保不超过最大超时
    if dataChannel.RetransmissionTimeout > config.MaxTransmissionTimeout {
        dataChannel.RetransmissionTimeout = config.MaxTransmissionTimeout  // 1秒
    }
}
```

### 11.5 消息重发调度器

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/datachannel/streaming.go#L334-363
func (dataChannel *DataChannel) ResendStreamDataMessageScheduler(log log.T) error {
    go func() {
        for {
            // 每 100ms 检查一次
            time.Sleep(config.ResendSleepInterval)  // 100ms
            
            dataChannel.OutgoingMessageBuffer.Mutex.Lock()
            streamMessageElement := dataChannel.OutgoingMessageBuffer.Messages.Front()
            dataChannel.OutgoingMessageBuffer.Mutex.Unlock()

            if streamMessageElement == nil {
                continue
            }

            streamMessage := streamMessageElement.Value.(StreamingMessage)
            
            // 检查是否超过重传超时
            if time.Since(streamMessage.LastSentTime) > dataChannel.RetransmissionTimeout {
                log.Debugf("Resend stream data message %d for the %d attempt.", 
                    streamMessage.SequenceNumber, *streamMessage.ResendAttempt)
                
                // 检查重发次数是否超限
                if *streamMessage.ResendAttempt >= config.ResendMaxAttempt {  // 3000次 ≈ 5分钟
                    log.Warnf("Message %d was resent over %d times.", 
                        streamMessage.SequenceNumber, config.ResendMaxAttempt)
                    dataChannel.isStreamMessageResendTimeout <- true  // 触发超时
                }
                
                *streamMessage.ResendAttempt++
                // 重发消息
                if err := SendMessageCall(log, dataChannel, streamMessage.Content, 
                    websocket.BinaryMessage); err != nil {
                    log.Errorf("Unable to send stream data message: %s", err)
                }
                streamMessage.LastSentTime = time.Now()
            }
        }
    }()
    return
}
```

---

## 12. 会话处理器

### 12.1 ShellSession 注册 (Plugin 端)

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/shellsession/shellsession.go#L37-56
type ShellSession struct {
    session.Session
    SizeData          message.SizeData  // 终端大小
    originalSttyState bytes.Buffer      // 原始 tty 状态
}

// 通过 init 函数自动注册
func init() {
    session.Register(&ShellSession{})
}

// 插件名称
func (ShellSession) Name() string {
    return config.ShellPluginName  // "Standard_Stream"
}
```

### 12.2 ShellSession 初始化

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/shellsession/shellsession.go#L58-65
func (s *ShellSession) Initialize(log log.T, sessionVar *session.Session) {
    s.Session = *sessionVar
    
    // 注册输出流处理器
    s.DataChannel.RegisterOutputStreamHandler(s.ProcessStreamMessagePayload, true)
    
    // 设置消息接收回调
    s.DataChannel.GetWsChannel().SetOnMessage(
        func(input []byte) {
            s.DataChannel.OutputMessageHandler(log, s.Stop, s.SessionId, input)
        })
}
```

### 12.3 设置会话处理器

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/shellsession/shellsession.go#L68-80
func (s *ShellSession) SetSessionHandlers(log log.T) (err error) {
    // 处理终端大小调整
    s.handleTerminalResize(log)

    // 处理控制信号 (Ctrl+C 等)
    s.handleControlSignals(log)

    // 处理键盘输入
    err = s.handleKeyboardInput(log)

    return
}
```

### 12.4 终端大小调整

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/shellsession/shellsession.go#L99-134
func (s *ShellSession) handleTerminalResize(log log.T) {
    go func() {
        for {
            // 获取当前终端大小
            width, height, err := GetTerminalSizeCall(int(os.Stdout.Fd()))
            if err != nil {
                width = 300
                height = 100
            }

            // 检查大小是否变化
            if s.SizeData.Rows != uint32(height) || s.SizeData.Cols != uint32(width) {
                sizeData := message.SizeData{
                    Cols: uint32(width),
                    Rows: uint32(height),
                }
                s.SizeData = sizeData

                // 发送大小变化消息
                inputSizeData, _ := json.Marshal(sizeData)
                log.Debugf("Sending input size data: %s", inputSizeData)
                if err = s.DataChannel.SendInputDataMessage(
                    log, message.Size, inputSizeData); err != nil {
                    log.Errorf("Failed to Send size data: %v", err)
                }
            }
            // 每 500ms 检查一次
            time.Sleep(ResizeSleepInterval)  // 500ms
        }
    }()
}
```

### 12.5 输出消息处理

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/shellsession/shellsession.go#L137-140
func (s ShellSession) ProcessStreamMessagePayload(
    log log.T, outputMessage message.ClientMessage) (isHandlerReady bool, err error) {
    
    // 将输出显示到终端
    s.DisplayMode.DisplayMessage(log, outputMessage)
    return true, nil
}
```

---

## 13. 错误处理与重连

### 13.1 Plugin 端重试配置

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/sessionhandler.go:36-41
s.retryParams = retry.RepeatableExponentialRetryer{
    GeometricRatio:      config.RetryBase,               // 2 (指数基数)
    InitialDelayInMilli: rand.Intn(config.DataChannelRetryInitialDelayMillis) + 
                         config.DataChannelRetryInitialDelayMillis,  // 100-200ms
    MaxDelayInMilli:     config.DataChannelRetryMaxIntervalMillis,   // 5000ms
    MaxAttempts:         config.DataChannelNumMaxRetries,            // 5
}
```

### 13.2 Agent 端重试配置

```go
// https://github.com/aws/amazon-ssm-agent/blob/mainline/agent/datachannel.go:296-303
retryer := retry.ExponentialRetryer{
    CallableFunc:        callable,
    GeometricRatio:      mgsConfig.RetryGeometricRatio,       // 2
    InitialDelayInMilli: rand.Intn(mgsConfig.DataChannelRetryInitialDelayMillis) + 
                         mgsConfig.DataChannelRetryInitialDelayMillis,
    MaxDelayInMilli:     mgsConfig.DataChannelRetryMaxIntervalMillis,
    MaxAttempts:         mgsConfig.DataChannelNumMaxAttempts,
    NonRetryableErrors:  getNonRetryableDataChannelErrors(),
}
```

### 13.3 指数退避算法

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/retry/retryer.go#L36-58
func (retryer *RepeatableExponentialRetryer) NextSleepTime(attempt int) time.Duration {
    // 计算: InitialDelay * (GeometricRatio ^ attempt)
    return time.Duration(
        float64(retryer.InitialDelayInMilli) * 
        math.Pow(retryer.GeometricRatio, float64(attempt))) * time.Millisecond
}

func (retryer *RepeatableExponentialRetryer) Call() (err error) {
    attempt := 0
    failedAttemptsSoFar := 0
    
    for {
        err := retryer.CallableFunc()
        if err == nil || failedAttemptsSoFar == retryer.MaxAttempts {
            return err
        }
        
        sleep := retryer.NextSleepTime(attempt)
        // 如果超过最大延迟，重置 attempt
        if int(sleep/time.Millisecond) > retryer.MaxDelayInMilli {
            attempt = 0
            sleep = retryer.NextSleepTime(attempt)
        }
        
        time.Sleep(sleep)
        attempt++
        failedAttemptsSoFar++
    }
}
```

### 13.4 Plugin 端 ResumeSession

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/sessionhandler.go:181-194
func (s *Session) ResumeSessionHandler(log log.T) (err error) {
    // 获取新的 Token
    s.TokenValue, err = s.GetResumeSessionParams(log)
    if err != nil {
        log.Errorf("Failed to get token: %v", err)
        return
    } else if s.TokenValue == "" {
        // 会话超时
        log.Debugf("Session: %s timed out", s.SessionId)
        fmt.Fprintf(os.Stdout, "Session: %s timed out.\n", s.SessionId)
        os.Exit(0)
    }
    
    // 更新 Token 并重连
    s.DataChannel.GetWsChannel().SetChannelToken(s.TokenValue)
    err = s.DataChannel.Reconnect(log)
    return
}
```

### 13.5 GetResumeSessionParams - 调用 ResumeSession API

```go
// https://github.com/aws/session-manager-plugin/blob/mainline/src/sessionhandler.go:130-178
func (s *Session) GetResumeSessionParams(log log.T) (string, error) {
    var (
        resumeSessionOutput *ssm.ResumeSessionOutput
        awsSession          *sdkSession.Session
        err                 error
    )

    // 检查 URL 是否已预签名
    presigned, _ := communicator.IsPresignedURL(s.StreamUrl)

    if !presigned {
        // 创建 SDK Session
        if awsSession, err = sdkutil.GetSessionWithQuickCheck(s.Endpoint); err != nil {
            return "", err
        }
        s.sdk = ssm.New(awsSession)
        
        // 设置签名器
        _, getCredErr := awsSession.Config.Credentials.Get()
        if getCredErr == nil {
            s.Signer = v4.NewSigner(awsSession.Config.Credentials)
        }
    } else {
        // 预签名 URL 不支持恢复会话
        return "", errors.New("Skip resuming session with presigned URL")
    }

    // 调用 ResumeSession API
    resumeSessionInput := ssm.ResumeSessionInput{
        SessionId: &s.SessionId,
    }

    resumeSessionOutput, err = s.sdk.ResumeSession(&resumeSessionInput)
    if err != nil {
        log.Errorf("Resume Session failed: %v", err)
        return "", err
    }

    if resumeSessionOutput.TokenValue == nil {
        return "", nil  // 会话已结束
    }

    return *resumeSessionOutput.TokenValue, nil
}
```

### 13.6 连接错误处理流程

```
WebSocket 连接错误
        │
        ▼
    OnError 回调触发
        │
        ▼
    创建 ExponentialRetryer
        │
        ▼
┌───────┴───────┐
│  重试循环      │
└──────┬───────┘
        │
        ▼
    调用 ResumeSession API
    获取新 Token
        │
        ├─── 成功: 设置新 Token
        │           │
        │           ▼
        │       调用 Reconnect
        │       关闭旧连接 + 打开新连接
        │           │
        │           ▼
        │       发送 OpenDataChannel
        │           │
        │           └─── 成功: 继续会话
        │
        ├─── 失败: 计算下次延迟
        │           │
        │           ▼
        │       Sleep(delay)
        │           │
        │           └─── 返回重试循环
        │
        └─── 超过 MaxAttempts: 退出
```

---

## 14. 配置参数汇总

### 14.1 Plugin 端配置 (config/config.go)

```go
const (
    // 服务名称
    ServiceName = "ssmmessages"         // V4 签名用的服务名

    // 角色
    RolePublishSubscribe = "publish_subscribe"  // 发布/订阅角色

    // 消息协议
    MessageSchemaVersion = "1.0"        // 消息模式版本

    // 超时配置
    DefaultTransmissionTimeout = 200 * time.Millisecond  // 默认重传超时
    DefaultRoundTripTime       = 100 * time.Millisecond  // 默认 RTT
    MaxTransmissionTimeout     = 1 * time.Second         // 最大重传超时
    ClockGranularity           = 10 * time.Millisecond   // 时钟粒度

    // RTT 计算常量
    RTTConstant  = 1.0 / 8.0   // alpha - RTT 平滑系数
    RTTVConstant = 1.0 / 4.0   // beta - RTTV 平滑系数
    DefaultRoundTripTimeVariation = 0

    // 重发配置
    ResendSleepInterval = 100 * time.Millisecond  // 重发检查间隔
    ResendMaxAttempt    = 3000                    // 最大重发次数 (约 5 分钟)

    // 缓冲区容量
    StreamDataPayloadSize         = 1024   // 流数据负载大小
    OutgoingMessageBufferCapacity = 10000  // 出站消息缓冲区容量
    IncomingMessageBufferCapacity = 10000  // 入站消息缓冲区容量

    // 重连配置
    RetryBase                          = 2     // 指数退避基数
    DataChannelNumMaxRetries           = 5     // 最大重试次数
    DataChannelRetryInitialDelayMillis = 100   // 初始重试延迟 (ms)
    DataChannelRetryMaxIntervalMillis  = 5000  // 最大重试间隔 (ms)
    RetryAttempt                       = 5     // WebSocket 读取重试次数

    // 心跳配置
    PingTimeInterval = 5 * time.Minute  // Ping 间隔

    // 插件名称
    ShellPluginName = "Standard_Stream"  // Shell 会话
    PortPluginName  = "Port"             // 端口转发会话
)
```

### 14.2 Agent 端配置 (session/config/config.go)

```go
const (
    // 通道类型
    ControlChannel = "control"           // 控制通道
    DataChannel    = "data"              // 数据通道

    // 角色
    RoleSubscribe       = "subscribe"       // 只接收 (控制通道)
    RolePublishSubscribe = "publish_subscribe"  // 发布/订阅

    // 重连配置 - 控制通道
    ControlChannelNumMaxRetries           = -1     // 无限重试
    ControlChannelRetryInitialDelayMillis = 5000   // 5秒
    ControlChannelRetryMaxIntervalMillis  = 300000 // 5分钟

    // 重连配置 - 数据通道
    DataChannelNumMaxAttempts            = 10     // 最大重试次数
    DataChannelRetryInitialDelayMillis   = 100    // 100ms
    DataChannelRetryMaxIntervalMillis    = 5000   // 5秒

    // RTT 和超时
    RetryGeometricRatio      = 2
    RetryJitterRatio         = 0.5
    DefaultRoundTripTime     = 200 * time.Millisecond
    DefaultTransmissionTimeout = 200 * time.Millisecond
    MaxTransmissionTimeout   = 1 * time.Second

    // 缓冲区
    OutgoingMessageBufferCapacity = 10000
    IncomingMessageBufferCapacity = 10000

    // 错误
    SessionAlreadyTerminatedError = "Session is already terminated"
)
```

### 14.3 关键超时参数对比

| 参数 | Plugin 端 | Agent 端 | 说明 |
|------|-----------|----------|------|
| 默认 RTT | 100ms | 200ms | 往返时间初始值 |
| 默认重传超时 | 200ms | 200ms | 消息重传超时 |
| 最大重传超时 | 1s | 1s | 重传超时上限 |
| Ping 间隔 | 5min | - | 心跳间隔 |
| 控制通道重连 | - | 无限/5min | Agent 保持连接 |
| 数据通道重连 | 5次/5s | 10次/5s | 会话重连 |
| 重发最大次数 | 3000 | - | 约 5 分钟 |
| 缓冲区容量 | 10000 | 10000 | 消息缓冲 |

---

## 总结

### 建连关键步骤回顾

1. **AWS CLI** 调用 `StartSession` API 获取 SessionId, StreamUrl, TokenValue
2. **AWS CLI** 启动 `session-manager-plugin` 子进程，传递会话参数
3. **Plugin** 解析参数，初始化 Session 和 DataChannel
4. **Plugin** 与 MGS 建立 WebSocket 连接（带 V4 签名或预签名 URL）
5. **Plugin** 发送 `OpenDataChannelInput` 完成通道握手
6. **Agent** 通过控制通道接收新会话通知，建立数据通道
7. **Agent** 发起握手协议，协商会话类型和加密
8. 握手完成后，**双向数据传输**开始

### 核心机制

- **序列号管理**: 确保消息顺序
- **ACK 确认**: 可靠传输
- **重传机制**: 处理丢包
- **KMS 加密**: 端到端加密
- **指数退避**: 重连策略

---

*文档生成时间: 2026-01-03*

