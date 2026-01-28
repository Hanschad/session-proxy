# Build stage
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /session-proxy ./cmd/session-proxy

# Runtime stage
FROM alpine:3.20

RUN apk add --no-cache ca-certificates

COPY --from=builder /session-proxy /usr/local/bin/session-proxy

# Default config location
VOLUME ["/config"]
VOLUME ["/root/.ssh"]
VOLUME ["/root/.aws"]

EXPOSE 28881

ENTRYPOINT ["session-proxy"]
CMD ["--config", "/config/config.yaml"]
