# AWS Session Connection Maintenance & High Availability Report

> "Bad programmers worry about the code. Good programmers worry about data structures and their relationships."

This report provides a meticulous analysis of how the AWS Session Manager Plugin and Agent establish relationships, maintain connection states, and handle network instability. All code references link firmly to the specific versions analyzed (`session-manager-plugin v1.2.0.0` and `amazon-ssm-agent v3.3.0.0`).

## 1. Connection Architecture

The connection is not a simple volatile socket; it is a persistent logical session overlaid on top of ephemeral WebSocket connections.

### 1.1 The Protocol Stack
1.  **Transport**: WebSocket (Secure, V4 Signed).
2.  **Session Layer**: Custom AWS Protocol (Sequence Numbers, ACKs).
3.  **Application**: Interactive Shell / Port Forwarding.

## 2. Connection Establishment & Maintenance

### 2.1 The Heartbeat (Application-Layer Keep-Alive)
To ensure the connection remains open through NATs and Load Balancers, the plugin implements an aggressive, application-layer heartbeat. It does not rely on TCP Keep-Alives.

*   **Logic**: A dedicated goroutine wakes up every `PingTimeInterval` to write a specific `websocket.PingMessage`.
*   **Code Reference**: [src/communicator/websocketchannel.go#L93-L112](https://github.com/aws/session-manager-plugin/blob/1.2.0.0/src/communicator/websocketchannel.go#L93-L112)

```go
// StartPings starts the pinging process to keep the websocket channel alive.
func (webSocketChannel *WebSocketChannel) StartPings(log log.T, pingInterval time.Duration) {
	go func() {
		for {
			if webSocketChannel.IsOpen == false {
				return
			}
			// ...
			err := webSocketChannel.Connection.WriteMessage(websocket.PingMessage, []byte("keepalive"))
			// ...
			time.Sleep(pingInterval)
		}
	}()
}
```

### 2.2 Reconnection (The "Resume" Strategy)
The system distinguishes between "Socket Failure" and "Session Failure". When the socket breaks, the session is *resumed*, not just restarted.

#### The Reconnection Loop
1.  **Trigger**: It invokes the `OnError` callback registered during initialization.
    *   [src/sessionmanagerplugin/session/sessionhandler.go#L91-L98](https://github.com/aws/session-manager-plugin/blob/1.2.0.0/src/sessionmanagerplugin/session/sessionhandler.go#L91-L98)

```go
	s.DataChannel.GetWsChannel().SetOnError(
		func(err error) {
			log.Errorf("Trying to reconnect the session: %v with seq num: %d", s.StreamUrl, s.DataChannel.GetStreamDataSequenceNumber())
			// Uses retryParams (ExponentialBackoff) to call ResumeSessionHandler
			s.retryParams.CallableFunc = func() (err error) { return s.ResumeSessionHandler(log) }
			if err = s.retryParams.Call(); err != nil {
				log.Error(err)
			}
		})
```

2.  **Action**: The `ResumeSessionHandler` is called. Significantly, this **refreshes the authentication token** via the AWS SDK `ResumeSession` API call before attempting to dial a new WebSocket.
    *   [src/sessionmanagerplugin/session/sessionhandler.go#L180-L194](https://github.com/aws/session-manager-plugin/blob/1.2.0.0/src/sessionmanagerplugin/session/sessionhandler.go#L180-L194)

```go
// ResumeSessionHandler gets token value and tries to Reconnect to datachannel
func (s *Session) ResumeSessionHandler(log log.T) (err error) {
	// 1. Get new token from AWS API
	s.TokenValue, err = s.GetResumeSessionParams(log)
	// ...
	// 2. Set new token on existing channel struct
	s.DataChannel.GetWsChannel().SetChannelToken(s.TokenValue)
	// 3. Reconnect the websocket
	err = s.DataChannel.Reconnect(log)
	return
}
```

## 3. High Availability (Data Integrity)

The system implements a Sliding Window-like protocol to ensure zero data loss during network blips or socket transitions.

### 3.1 Data Structures
Two primary structures govern data integrity:
1.  **OutgoingMessageBuffer**: A Linked List (`list.List`) holding messages sent but not yet acknowledged (ACK'd).
2.  **IncomingMessageBuffer**: A Map (`map[int64]StreamingMessage`) to buffer out-of-order packets before processing.

### 3.2 Retransmission Logic (The "Scheduler")
A background goroutine (`ResendStreamDataMessageScheduler`) continuously monitors the `OutgoingMessageBuffer`.

*   **Condition**: If `time.Since(LastSentTime) > RetransmissionTimeout`, the message is resent.
*   **Code Reference**: [src/datachannel/streaming.go#L332-L360](https://github.com/aws/session-manager-plugin/blob/1.2.0.0/src/datachannel/streaming.go#L332-L360)

```go
func (dataChannel *DataChannel) ResendStreamDataMessageScheduler(log log.T) (err error) {
	go func() {
		for {
			time.Sleep(config.ResendSleepInterval)
			// ... locking ...
			streamMessageElement := dataChannel.OutgoingMessageBuffer.Messages.Front()
			// ...
			streamMessage := streamMessageElement.Value.(StreamingMessage)
			if time.Since(streamMessage.LastSentTime) > dataChannel.RetransmissionTimeout {
				// Resend logic
				*streamMessage.ResendAttempt++
				if err = SendMessageCall(log, dataChannel, streamMessage.Content, websocket.BinaryMessage); err != nil {
                     // ...
				}
				streamMessage.LastSentTime = time.Now()
			}
		}
	}()
	return
}
```

### 3.3 Dynamic Timeout Calculation (TCP-like Adaptation)
To accommodate varying network latencies, the Agent dynamically calculates the `RetransmissionTimeout` based on the Round Trip Time (RTT) of previous ACKs. This prevents premature retransmissions on slow networks.

*   **Algorithm**: `SRTT` (Smoothed RTT) and `RTTVAR` (RTT Variation) similar to TCP RFC 6298.
*   **Code Reference**: [agent/session/datachannel/datachannel.go#L667-L691](https://github.com/aws/amazon-ssm-agent/blob/3.3.0.0/agent/session/datachannel/datachannel.go#L667-L691)

```go
func (dataChannel *DataChannel) calculateRetransmissionTimeout(log log.T, streamingMessage StreamingMessage) {
	newRoundTripTime := float64(time.Since(streamingMessage.LastSentTime))

	// Update RTT Variation
	dataChannel.RoundTripTimeVariation = ((1 - mgsConfig.RTTVConstant) * dataChannel.RoundTripTimeVariation) +
		(mgsConfig.RTTVConstant * math.Abs(dataChannel.RoundTripTime-newRoundTripTime))

	// Update Smoothed RTT
	dataChannel.RoundTripTime = ((1 - mgsConfig.RTTConstant) * dataChannel.RoundTripTime) +
		(mgsConfig.RTTConstant * newRoundTripTime)

	// Calculate Timeout: SRTT + 4*RTTVAR
	dataChannel.RetransmissionTimeout = time.Duration(dataChannel.RoundTripTime +
		math.Max(float64(mgsConfig.ClockGranularity), float64(4*dataChannel.RoundTripTimeVariation)))

	// ... Cap at max timeout ...
}
```

## 4. Handling Network Anomalies

### 4.1 Packet Loss & Out-of-Order Delivery
*   **Scenario**: Sequence 1 arrives, then Sequence 3.
*   **Handling**: 
    1. Agent sees `Sequence 3 > ExpectedSequence 2`.
    2. Sequence 3 is stored in `IncomingMessageBuffer`.
    3. An ACK is sent for Sequence 3 (Preventing sender retransmission).
    4. When Sequence 2 arrives, it is processed, and then the buffer is drained sequentially.
*   **Code Reference**: [agent/session/datachannel/datachannel.go#L719-L741](https://github.com/aws/amazon-ssm-agent/blob/3.3.0.0/agent/session/datachannel/datachannel.go#L719-L741)

### 4.2 Stream Timeout (Zombie Session Prevention)
To ensure resources aren't wasted on dead connections, the plugin tracks a message resend timeout. If a message is resent `ResendMaxAttempt` times without success, the session is terminated.

*   **Code Reference**: [src/sessionmanagerplugin/session/session.go#L106-L121](https://github.com/aws/session-manager-plugin/blob/1.2.0.0/src/sessionmanagerplugin/session/session.go#L106-L121)

```go
var handleStreamMessageResendTimeout = func(session *Session, log log.T) {
	go func() {
		for {
			time.Sleep(config.ResendSleepInterval)
			if <-session.DataChannel.IsStreamMessageResendTimeout() {
				log.Errorf("Terminating session %s as the stream data was not processed before timeout.", session.SessionId)
				if err := session.TerminateSession(log); err != nil {
                    // ...
				}
				return
			}
		}
	}()
}
```

## 5. ResumeSession for SSH Tunnels (Corrected)

Earlier experiments concluded that `ResumeSession` does not work for SSH-over-SSM tunnels. That result came from **re-running the SSH handshake** after resume. The agent-side TCP connection to `sshd` still holds the existing SSH session; attempting a fresh SSH handshake on top of a resumed SSM channel will hang.

The correct model matches the official session-manager-plugin:

| Layer | On WebSocket drop | Action |
|-------|-------------------|--------|
| SSM WebSocket | Broken | `ResumeSession` → new token → re-dial WebSocket → `OpenDataChannel` |
| SSM sequence state | Preserved | Keep `seqNum`, unacked `outgoing`, `expectedSeqNum` |
| SSH session | Preserved | **Do not** close `sshClient` or re-handshake |
| User TCP streams | Preserved | Adapter `net.Conn` stays open; unacked frames retransmit |

`session-proxy` now implements this transparent WebSocket resume when `WithResume` is wired from the upstream pool.

### Environment variables

- `SESSION_PROXY_SSM_RESUME=off` — disable resume; transport failures close the adapter (legacy behavior).
- `SESSION_PROXY_SSM_RESUME_BUDGET` — total time budget for resume attempts (default `90s`).

### Pool replenishment vs resume

- **Resume** — same SSM session + same SSH connection; for transient WebSocket loss.
- **Replenish (`StartSession`)** — new SSM session + new SSH handshake; for dead SSH transport or adapter budget exhaustion.

### Validation experiment

Run against a real environment:

```bash
go run ./cmd/resume-experiment --config config.yaml --upstream prod
```

The tool opens an SSH session that prints `tick-N` every second, force-closes the WebSocket after 10s, and reports `PASS` if ticks continue without a new SSH session.

## Verdict

The implementation prioritizes **Session Continuity** over **Connection Stability**. Transport-layer reliability (sequence numbers, ACKs, buffering, resume) keeps long-lived SSH and database connections alive across transient WebSocket loss, while pool replenishment handles dead transports.

