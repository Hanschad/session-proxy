# Architecture Design: Session Proxy

## Overview
`session-proxy` is a unified CLI tool designed to simplify secure connectivity to private AWS EC2 instances. It replaces the complex two-step process of using the AWS CLI `ProxyCommand` with a standard SSH client.

**Core Value Proposition**:
- Single binary deployment (Go).
- Native AWS SSM integration (no external `session-manager-plugin` required).
- Integrated SOCKS5 proxy server.

## High-Level Architecture

```mermaid
graph TD
    User["User/Browser"] -->|SOCKS5 Request| SocksServer[("SOCKS5 Server (Listener)")]
    SocksServer -->|Encrypted SSH Traffic| SSHClient["Internal SSH Client"]
    SSHClient -->|Raw Bytes| VirtConn["Virtual Connection Adapter"]
    VirtConn -->|SSM Protocol Frames| WSClient["WebSocket Client"]
    WSClient -->|WSS| AWSSSM["AWS SSM Service"]
    AWSSSM -->|WSS| SSMAgent["SSM Agent (on EC2)"]
    SSMAgent -->|TCP| TargetDest["Target Destination (via Connect)"]
```

## Component Design

### 1. Application Layer (CLI)
Entry point responsible for:
- Parsing command-line arguments (target instance, ports, SSH keys).
- Initializing the AWS SDK v2 configuration.
- Wiring together the components.

### 2. SSM Connector (`internal/aws/ssm`)
Responsible for the specific interaction with AWS APIs.
- **Input**: Instance ID, verify target status.
- **Action**: Call `StartSession` with document `AWS-StartSSHSession`.
- **Output**: Session Token, Stream URL.

### 3. Protocol Adapter (`internal/protocol`)
The core innovation piece. It translates between Go's standard `net.Conn` stream and the message-based AWS SSM Protocol.

**Key responsibilities:**
- **Framing**: Serializing/Deserializing the SSM binary protocol (Payload type, Sequence number, Flags).
- **Buffering**: Handling the difference between stream-based SSH reads and packet-based SSM payloads.
- **Heartbeats**: Maintaining the WebSocket connection.

**Interface:**
```go
type SSMConnection interface {
    net.Conn // Implements Read/Write/Close/LocalAddr/RemoteAddr...
}
```

**Protocol Details**:
The adapter must handle the following payload types:
- `Output_Stream_Data`: Received from AWS (Remote data).
- `Input_Stream_Data`: Sent to AWS (User data).
- `Acknowledge`: Handling flow control if necessary.

### 4. SSH Tunnel (`internal/ssh`)
Wraps the `SSMConnection` with an SSH handshake.
- Uses `golang.org/x/crypto/ssh`.
- Performs authentication (Private Key/Agent).
- Establishes the secure tunnel *inside* the SSM session.

### 5. SOCKS5 Proxy (`internal/proxy`)
Listens on a local port (e.g., 28881) and proxies traffic via the SSH Tunnel.
- Validates SOCKS5 handshake.
- Uses the `SSHClient.Dial` to forward connections, effectively making the EC2 instance the exit node.

## Data Flow Sequence

```mermaid
sequenceDiagram
    participant User
    participant Proxy as session-proxy
    participant AWS as AWS API
    participant EC2 as EC2 (SSM Agent)

    Note over User, Proxy: Initialization
    User->>Proxy: Run (Target: i-xxx)
    Proxy->>AWS: StartSession(i-xxx)
    AWS-->>Proxy: Session Token & WSS URL

    Note over Proxy, EC2: Transport Establishment
    Proxy->>AWS: WebSocket Connect
    AWS->>EC2: Signal Session Start
    Proxy->>Proxy: Upgrade WS to net.Conn
    Proxy->>EC2: SSH Handshake (over WS)
    EC2-->>Proxy: SSH Accepted

    Note over User, Proxy: Traffic Forwarding
    User->>Proxy: SOCKS5 Connect (google.com:80)
    Proxy->>EC2: SSH Direct-TCP-IP (google.com:80)
    EC2->>Internet: Connect google.com:80
    Internet-->>EC2: Response
    EC2-->>Proxy: Encrypted SSH Frame
    Proxy-->>User: SOCKS5 Data
```


### 6. Low-Level Protocol Specification

The SSM Agent communication uses a binary message format over WebSocket.

**Message Structure:**
```
[HeaderLength (4 bytes, BigEndian)]
[Header (JSON string of HeaderLength bytes)]
[PayloadLength (4 bytes, BigEndian)]
[Payload (Bytes of PayloadLength bytes)]
```

**Header Format:**
```json
{
    "MessageType": "output_stream_data" | "input_stream_data" | "acknowledge",
    "SchemaVersion": 1,
    "CreatedDate": 1234567890,
    "SequenceNumber": 0,
    "Flags": 0,
    "MessageId": "uuid"
}
```

**Payload Handling:**
- **Input (Client -> Agent)**: Raw bytes from the SSH client are wrapped in `input_stream_data`.
- **Output (Agent -> Client)**: `output_stream_data` payload is unwrapped and written to the SSH client.
- **Acknowledge**: Must be sent/received to confirm message delivery sequence.

## Security Considerations

1.  **IAM Permissions**:
    The user's AWS credential must have `ssm:StartSession` permission on the target resource.
    
    ```json
    {
        "Effect": "Allow",
        "Action": "ssm:StartSession",
        "Resource": "arn:aws:ec2:region:account:instance/i-xxx",
        "Condition": {
            "StringEquals": {
                "ssm:document/name": "AWS-StartSSHSession"
            }
        }
    }
    ```

2.  **End-to-End Encryption**:
    - **Layer 1 (TLS)**: WebSocket connection to AWS is TLS encrypted.
    - **Layer 2 (SSH)**: The inner tunnel is standard SSH (AES/ChaCha20), ensuring AWS cannot inspect the traffic content, only the metadata.

3.  **Host Key Verification**:
    The SSH client *must* perform host key verification. Since we are connecting via a proxy, standard `known_hosts` checks apply. Users must verify the fingerprint upon first connection.

## Directory Structure Strategy

```text
/
├── cmd/
│   └── session-proxy/    # Main entry point
├── internal/
│   ├── aws/              # AWS SDK interactions (SSM StartSession)
│   ├── protocol/         # SSM <-> net.Conn adapter (Reads/Writes frames)
│   ├── ssh/              # SSH Client wrapper & Dial logic
│   └── proxy/            # SOCKS5 Server implementation
├── pkg/                  # (Optional) Reusable libraries
└── docs/                 # Documentation
```
