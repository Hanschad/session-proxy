# Session-Proxy

AWS SSM Session Manager over SSH with SOCKS5 proxy support.

## Features

- 🔐 SSH tunneling through AWS SSM (no bastion host needed)
- 🌐 SOCKS5 proxy with multi-upstream routing
- 🔄 Automatic reconnection on connection loss
- 🎯 CIDR and domain-based route matching
- 🔑 Optional SOCKS5 authentication
- ♻️ Hot-reload for routes configuration

## Quick Start

```bash
# Install
go install github.com/hanschad/session-proxy/cmd/session-proxy@latest

# Legacy single-target mode
session-proxy --target i-0123456789abcdef0 --region us-east-1

# Multi-upstream mode with config file
session-proxy --config config.yaml
```

## Configuration

See [config.example.yaml](config.example.yaml) for full options.

```yaml
listen: "127.0.0.1:28881"

upstreams:
  prod:
    ssh:
      user: ec2-user
      key: ~/.ssh/id_rsa
    aws:
      profile: production
    instances:
      - i-prod-instance-1
      - i-prod-instance-2

routes:
  - match: "10.0.0.0/8"
    upstream: prod
  - match: "*.internal.company.com"
    upstream: prod

default: prod
```

## Usage

```bash
# With SOCKS5 auth
session-proxy --auth-user admin --auth-pass secret

# Debug mode
session-proxy --debug

# Write logs to file (append mode)
session-proxy --log-file /var/log/session-proxy.log

# Show effective config
session-proxy --print-config
```

### Logging

- Default: logs are written to `stderr` (terminal).
- File logging: set `--log-file`, `log_file` in config, or `SESSION_PROXY_LOG_FILE`.
- Log file is opened in append mode and parent directory is created automatically if needed.

### Using with curl

```bash
curl --proxy socks5h://127.0.0.1:28881 http://internal-service:8080
```

### Using with SSH

```bash
ssh -o ProxyCommand="nc -X 5 -x 127.0.0.1:28881 %h %p" user@internal-host
```

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      session-proxy                          │
├─────────────────────────────────────────────────────────────┤
│  SOCKS5 Server (:28881)                                     │
│       │                                                     │
│       ▼                                                     │
│  Router (CIDR/Domain matching)                              │
│       │                                                     │
│       ▼                                                     │
│  Upstream Pool (SSH over SSM WebSocket)                     │
│       │                                                     │
│       ▼                                                     │
│  AWS SSM Session Manager ──────► EC2 Instance               │
└─────────────────────────────────────────────────────────────┘
```

## Docker

```bash
# Pull from GitHub Container Registry
docker pull ghcr.io/hanschad/session-proxy:latest

# Run with config
docker run -d \
  -p 28881:28881 \
  -v $(pwd)/config.yaml:/config/config.yaml:ro \
  -v ~/.ssh:/root/.ssh:ro \
  -v ~/.aws:/root/.aws:ro \
  ghcr.io/hanschad/session-proxy:latest

# Or use docker compose
docker compose up -d

# View logs
docker compose logs -f
```

## Development

```bash
# Build
make build

# Test
make test

# Lint
make lint
```

## License

MIT License - see [LICENSE](LICENSE)
