package protocol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/hanschad/session-proxy/internal/retry"
)

var errStaleConn = errors.New("stale websocket generation")

var adapterDialWebSocketHook = func(ctx context.Context, streamURL string) (*websocket.Conn, error) {
	netDialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: tcpKeepAlive,
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		NetDialContext:   netDialer.DialContext,
		Proxy:            http.ProxyFromEnvironment,
	}
	parsedURL, err := url.Parse(streamURL)
	if err != nil {
		return nil, fmt.Errorf("parse stream url: %w", err)
	}
	wsConn, _, err := dialer.DialContext(ctx, parsedURL.String(), nil)
	return wsConn, err
}

func applyAdapterOptions(a *Adapter, opts []AdapterOption) {
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	a.resumeBudget = defaultResumeBudget
	if v := os.Getenv("SESSION_PROXY_SSM_RESUME_BUDGET"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			a.resumeBudget = d
		} else {
			log.Printf("[WARN] SESSION_PROXY_SSM_RESUME_BUDGET=%q invalid, using default %s", v, defaultResumeBudget)
		}
	}
	if os.Getenv("SESSION_PROXY_SSM_RESUME") == "off" {
		a.resumeDisabled = true
	}
	if a.connGen == 0 {
		a.connGen = 1
	}
}

func (a *Adapter) resumeEnabled() bool {
	return a.resumeFunc != nil && !a.resumeDisabled
}

// Reconnecting reports whether the adapter is swapping in a new WebSocket.
func (a *Adapter) Reconnecting() bool {
	a.reconnectMu.Lock()
	defer a.reconnectMu.Unlock()
	return a.reconnecting
}

// ForceCloseTransport closes only the underlying WebSocket. Used by resume
// validation tooling to simulate a transport drop without tearing down SSH state.
func (a *Adapter) ForceCloseTransport() {
	conn, _ := a.getConn()
	if conn != nil {
		_ = conn.Close()
	}
}

func (a *Adapter) currentConnGen() uint64 {
	a.connMu.RLock()
	defer a.connMu.RUnlock()
	return a.connGen
}

func (a *Adapter) connGenerationStale(gen uint64) bool {
	return a.currentConnGen() != gen
}

func (a *Adapter) getConn() (*websocket.Conn, uint64) {
	a.connMu.RLock()
	defer a.connMu.RUnlock()
	return a.conn, a.connGen
}

func (a *Adapter) swapConn(ws *websocket.Conn) uint64 {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if a.conn != nil {
		_ = a.conn.Close()
	}
	a.conn = ws
	a.connGen++
	gen := a.connGen
	a.installPongHandler(ws, gen)
	return gen
}

func (a *Adapter) installPongHandler(ws *websocket.Conn, gen uint64) {
	_ = ws.SetReadDeadline(time.Now().Add(pongWait))
	ws.SetPongHandler(func(appData string) error {
		a.connMu.RLock()
		current := a.conn
		currentGen := a.connGen
		a.connMu.RUnlock()
		if currentGen != gen || current != ws {
			return nil
		}
		_ = ws.SetReadDeadline(time.Now().Add(pongWait))
		debugLog("WebSocket Pong received")
		return nil
	})
}

func (a *Adapter) sendOpenDataChannel(ws *websocket.Conn, token string) error {
	initMsg := map[string]string{
		"MessageSchemaVersion": "1.0",
		"RequestId":            CleanUUID(uuid.New()),
		"TokenValue":           token,
		"ClientId":             a.clientId,
		"ClientVersion":        ClientVersion,
	}
	debugLog("Sending OpenDataChannel: %+v", initMsg)
	_ = ws.SetWriteDeadline(time.Now().Add(writeWait))
	return ws.WriteJSON(initMsg)
}

func (a *Adapter) waitUntilConnected() error {
	for {
		select {
		case <-a.done:
			return io.ErrClosedPipe
		default:
		}

		a.reconnectMu.Lock()
		reconnecting := a.reconnecting
		gate := a.reconnectGate
		a.reconnectMu.Unlock()

		if !reconnecting {
			return nil
		}
		if gate == nil {
			if err := a.sleepWithWriteDeadline(10 * time.Millisecond); err != nil {
				return err
			}
			continue
		}
		if err := a.waitWithWriteDeadline(gate); err != nil {
			return err
		}
	}
}

// handleTransportFailure initiates resume when enabled. Returns true if the
// adapter is permanently closing.
func (a *Adapter) handleTransportFailure(err error) bool {
	// A deliberate Close() also surfaces as a transport error to the read/ping
	// loops; do not start (or log) a resume for an already-closed adapter.
	select {
	case <-a.done:
		return true
	default:
	}
	if !a.resumeEnabled() {
		if a.streamWriter != nil {
			_ = a.streamWriter.Close()
		}
		a.closeWithError(err)
		return true
	}
	a.startReconnect(err)
	return false
}

func (a *Adapter) startReconnect(trigger error) {
	select {
	case <-a.done:
		return
	default:
	}
	if !a.resumeEnabled() {
		return
	}

	a.reconnectMu.Lock()
	if a.reconnecting {
		a.reconnectMu.Unlock()
		return
	}
	a.reconnecting = true
	a.reconnectGate = make(chan struct{})
	a.reconnectMu.Unlock()

	log.Printf("[WARN] SSM WebSocket transport failure, attempting resume: %v", trigger)
	go a.reconnectLoop(trigger)
}

func (a *Adapter) reconnectLoop(trigger error) {
	defer func() {
		a.reconnectMu.Lock()
		a.reconnecting = false
		gate := a.reconnectGate
		a.reconnectGate = nil
		a.reconnectMu.Unlock()
		if gate != nil {
			select {
			case <-gate:
				// Already closed by Adapter.Close().
			default:
				close(gate)
			}
		}
	}()

	budget := a.resumeBudget
	if budget <= 0 {
		budget = defaultResumeBudget
	}
	deadline := time.Now().Add(budget)
	retryer := retry.DefaultRetryer()
	retryer.InitialDelay = 500 * time.Millisecond
	retryer.MaxDelay = 5 * time.Second
	retryer.MaxAttempts = 0

	attempt := 0
	for time.Now().Before(deadline) {
		select {
		case <-a.done:
			return
		default:
		}

		attempt++
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		callTimeout := remaining
		if callTimeout > 30*time.Second {
			callTimeout = 30 * time.Second
		}

		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		streamURL, token, err := a.resumeFunc(ctx)
		cancel()
		if err != nil {
			debugLog("ResumeSession attempt %d failed: %v", attempt, err)
			if !a.sleepResumeBackoff(retryer, attempt, deadline) {
				break
			}
			continue
		}

		dialCtx, dialCancel := context.WithTimeout(context.Background(), callTimeout)
		ws, err := adapterDialWebSocketHook(dialCtx, streamURL)
		dialCancel()
		if err != nil {
			debugLog("Resume websocket dial attempt %d failed: %v", attempt, err)
			if !a.sleepResumeBackoff(retryer, attempt, deadline) {
				break
			}
			continue
		}

		if err := a.sendOpenDataChannel(ws, token); err != nil {
			_ = ws.Close()
			debugLog("Resume OpenDataChannel attempt %d failed: %v", attempt, err)
			if !a.sleepResumeBackoff(retryer, attempt, deadline) {
				break
			}
			continue
		}

		gen := a.swapConn(ws)
		a.markOutgoingForRetransmit()
		a.clearPauseForReconnect()
		a.markHandshakeComplete()

		go a.readLoopForGen(gen)
		go a.pingLoopForGen(gen)

		log.Printf("[INFO] SSM WebSocket resumed successfully after transport failure")
		return
	}

	if a.streamWriter != nil {
		_ = a.streamWriter.Close()
	}
	a.closeWithError(fmt.Errorf("%w: %v", errResumeBudgetExhausted, trigger))
}

func (a *Adapter) sleepResumeBackoff(retryer *retry.ExponentialRetryer, attempt int, deadline time.Time) bool {
	delay := retryer.NextDelay(attempt)
	if delay > time.Until(deadline) {
		delay = time.Until(deadline)
	}
	if delay <= 0 {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-a.done:
		return false
	case <-timer.C:
		return true
	}
}

func (a *Adapter) markOutgoingForRetransmit() {
	a.outgoingMu.Lock()
	defer a.outgoingMu.Unlock()

	forceResendAt := time.Now().Add(-defaultRetransmissionTimeout - time.Millisecond)
	for _, om := range a.outgoing {
		om.lastSent = forceResendAt
		om.resendCount = 0
	}
	if a.outgoingCond != nil {
		a.outgoingCond.Broadcast()
	}
}

func (a *Adapter) clearPauseForReconnect() {
	a.pauseMu.Lock()
	defer a.pauseMu.Unlock()

	if !a.paused {
		return
	}
	a.paused = false
	a.pausedSince = time.Time{}
	if a.pauseCh != nil {
		close(a.pauseCh)
		a.pauseCh = nil
	}
}

func (a *Adapter) readLoopForGen(gen uint64) {
	for {
		if a.connGenerationStale(gen) {
			return
		}

		msg, err := a.readMessageForGen(gen)
		if err != nil {
			if a.connGenerationStale(gen) {
				return
			}
			if a.handleTransportFailure(err) {
				return
			}
			return
		}
		if msg == nil {
			continue
		}
		if !a.dispatchMessage(msg) {
			return
		}
	}
}

func (a *Adapter) readMessageForGen(gen uint64) (*AgentMessage, error) {
	conn, currentGen := a.getConn()
	if conn == nil {
		return nil, io.ErrClosedPipe
	}
	if currentGen != gen {
		return nil, errStaleConn
	}

	_, msgBytes, err := conn.ReadMessage()
	if err != nil {
		log.Printf("[WARN] WS Read Error: %v", err)
		return nil, err
	}

	agentMsg, err := UnmarshalMessage(msgBytes)
	if err != nil {
		debugLog("Unmarshal Error: %v", err)
		return nil, nil
	}

	sample := true
	switch agentMsg.Header.MessageType {
	case MsgTypeOutputStreamData, MsgTypeInputStreamData:
		sample = agentMsg.Header.SequenceNumber%rxFrameLogEveryN == 0
	case MsgTypeAcknowledge:
		a.rxAckCount++
		sample = a.rxAckCount%rxFrameLogEveryN == 0
	}
	if sample {
		debugLog("RX Frame: Type=%s Seq=%d Len=%d Flags=%d PayloadType=%d HL=%d MsgId=%s",
			agentMsg.Header.MessageType,
			agentMsg.Header.SequenceNumber,
			len(agentMsg.Payload),
			agentMsg.Header.Flags,
			agentMsg.Header.PayloadType,
			agentMsg.Header.HeaderLength,
			agentMsg.Header.MessageId.String())
	}

	if agentMsg.Header.MessageType == MsgTypeOutputStreamData {
		if len(agentMsg.Payload) <= maxTextPayloadLogBytes && looksMostlyText(agentMsg.Payload) {
			debugLog("Output Payload (text): %q", string(agentMsg.Payload))
		}
	}

	return agentMsg, nil
}

func (a *Adapter) pingLoopForGen(gen uint64) {
	debugLog("Ping loop started for gen=%d interval=%v", gen, PingInterval)
	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.done:
			debugLog("Stopping ping loop - adapter closed")
			return
		case <-ticker.C:
		}

		if a.connGenerationStale(gen) {
			return
		}

		err := a.sendPingForGen(gen)
		if err != nil {
			if a.connGenerationStale(gen) {
				return
			}
			debugLog("WebSocket Ping failed (first attempt): %v", err)
			if pingRetryErr := a.sleepWithWriteDeadline(500 * time.Millisecond); pingRetryErr != nil {
				if a.handleTransportFailure(fmt.Errorf("websocket ping failed before retry: %w", err)) {
					return
				}
				return
			}

			err = a.sendPingForGen(gen)
			if err != nil {
				if a.connGenerationStale(gen) {
					return
				}
				debugLog("WebSocket Ping failed (retry): %v", err)
				if a.handleTransportFailure(fmt.Errorf("websocket ping failed after retry: %w", err)) {
					return
				}
				return
			}
		}
		debugLog("WebSocket Ping sent")
	}
}

func (a *Adapter) sendPingForGen(gen uint64) error {
	conn, currentGen := a.getConn()
	if conn == nil {
		return io.ErrClosedPipe
	}
	if currentGen != gen {
		return errStaleConn
	}

	deadline := time.Now().Add(writeWait)
	a.writeMu.Lock()
	err := conn.WriteControl(websocket.PingMessage, []byte("keepalive"), deadline)
	a.writeMu.Unlock()
	return err
}
