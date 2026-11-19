package qqbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The gateway protocol: https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-framework/event-emit.html.
//
// connect → Hello (heartbeat interval) → Identify, or Resume with the last
// session id and sequence → READY / RESUMED → Dispatch events, heartbeats
// acknowledged in between. Reconnect (op 7) asks for a resume; Invalid Session
// (op 9) for a fresh identify.

// Gateway close codes that decide what a reconnect does.
var (
	// The session cannot be resumed: identify again.
	closeNewSession = map[int]bool{4006: true, 4007: true, 4009: true}
	// The token was refused.
	closeBadToken = map[int]bool{4004: true}
	// Too many connection attempts: wait longer.
	closeRateLimited = map[int]bool{4008: true}
	// The bot cannot connect at all until an operator acts: invalid or
	// disallowed intents, banned or offline bot.
	closeFatal = map[int]bool{4010: true, 4011: true, 4012: true, 4013: true, 4014: true, 4914: true, 4915: true}
)

var (
	errReconnect      = errors.New("gateway asked for a reconnect")
	errInvalidSession = errors.New("gateway invalidated the session")
	errZombie         = errors.New("heartbeat not acknowledged")
)

// runGateway keeps a gateway connection up until ctx ends, reconnecting with
// backoff. It returns early only for a close the bot cannot recover from.
func (d *Driver) runGateway(ctx context.Context) {
	delay := d.retryBase
	for ctx.Err() == nil {
		started := time.Now()
		err := d.connect(ctx)
		if ctx.Err() != nil {
			return
		}
		var ce *websocket.CloseError
		switch {
		case errors.As(err, &ce) && closeFatal[ce.Code]:
			d.log.Error("qq gateway closed for good; fix the bot's configuration and restart", "code", ce.Code, "reason", ce.Text)
			return
		case errors.As(err, &ce) && closeNewSession[ce.Code], errors.Is(err, errInvalidSession):
			d.resetSession()
		case errors.As(err, &ce) && closeBadToken[ce.Code]:
			d.tokens.invalidate()
			d.resetSession()
		case errors.As(err, &ce) && closeRateLimited[ce.Code]:
			delay = maxDuration(delay, 30*d.retryBase)
		}
		if time.Since(started) > time.Minute {
			delay = d.retryBase // a connection that lived was healthy; start over
		}
		d.log.Warn("qq gateway disconnected; reconnecting", "err", err, "in", delay)
		jitter := time.Duration(rand.Int64N(int64(delay)/4 + 1))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay + jitter):
		}
		delay = minDuration(delay*2, 60*d.retryBase)
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func (d *Driver) resetSession() {
	d.gwMu.Lock()
	d.sessionID, d.seq = "", 0
	d.gwMu.Unlock()
}

// gatewayURL looks up the websocket address.
func (d *Driver) gatewayURL(ctx context.Context) (string, error) {
	if d.wsURL != "" {
		return d.wsURL, nil
	}
	var out struct {
		URL string `json:"url"`
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := d.call(cctx, "GET", "/gateway/bot", nil, &out); err != nil {
		return "", fmt.Errorf("gateway lookup: %w", err)
	}
	if out.URL == "" {
		return "", fmt.Errorf("gateway lookup returned no url")
	}
	return out.URL, nil
}

// wsConn serialises writes: the read loop and the heartbeat both send.
type wsConn struct {
	mu sync.Mutex
	c  *websocket.Conn
}

func (w *wsConn) send(op int, data any) error {
	b, err := json.Marshal(map[string]any{"op": op, "d": data})
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return w.c.WriteMessage(websocket.TextMessage, b)
}

// connect runs one gateway connection until it ends, and says why.
func (d *Driver) connect(ctx context.Context) error {
	addr, err := d.gatewayURL(ctx)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	c, _, err := dialer.DialContext(ctx, addr, nil)
	if err != nil {
		return fmt.Errorf("gateway dial: %w", err)
	}
	conn := &wsConn{c: c}
	defer c.Close()
	// Closing the socket is how ctx cancellation reaches the blocked read.
	stop := context.AfterFunc(ctx, func() {
		conn.mu.Lock()
		_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		conn.mu.Unlock()
		c.Close()
	})
	defer stop()

	// Hello comes first and carries the heartbeat interval.
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	var hello payload
	if err := c.ReadJSON(&hello); err != nil {
		return fmt.Errorf("gateway hello: %w", err)
	}
	if hello.Op != opHello {
		return fmt.Errorf("gateway sent op %d before hello", hello.Op)
	}
	var hd struct {
		HeartbeatInterval int64 `json:"heartbeat_interval"`
	}
	_ = json.Unmarshal(hello.D, &hd)
	interval := time.Duration(hd.HeartbeatInterval) * time.Millisecond
	if interval <= 0 {
		interval = 45 * time.Second
	}

	tok, err := d.tokens.Token(ctx)
	if err != nil {
		return err
	}
	d.gwMu.Lock()
	sessionID, seq := d.sessionID, d.seq
	d.gwMu.Unlock()
	if sessionID != "" {
		err = conn.send(opResume, map[string]any{"token": "QQBot " + tok, "session_id": sessionID, "seq": seq})
	} else {
		err = conn.send(opIdentify, map[string]any{
			"token":   "QQBot " + tok,
			"intents": d.intents(),
			"shard":   []int{0, 1},
			"properties": map[string]string{
				"$os":      runtime.GOOS,
				"$browser": "agentflow",
				"$device":  "agentflow",
			},
		})
	}
	if err != nil {
		return fmt.Errorf("gateway identify: %w", err)
	}

	// Heartbeats run beside the read loop. An unacknowledged beat means the
	// connection is dead even if TCP has not noticed; the socket is closed so
	// the read fails and the session resumes on a new connection.
	var acked sync.Mutex
	pending := false
	hbErr := make(chan error, 1)
	hbDone := make(chan struct{})
	defer close(hbDone)
	beat := func() error {
		d.gwMu.Lock()
		var s any
		if d.seq > 0 {
			s = d.seq
		}
		d.gwMu.Unlock()
		return conn.send(opHeartbeat, s)
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-t.C:
				acked.Lock()
				missed := pending
				pending = true
				acked.Unlock()
				if missed {
					hbErr <- errZombie
					c.Close()
					return
				}
				if err := beat(); err != nil {
					hbErr <- err
					c.Close()
					return
				}
			}
		}
	}()

	for {
		// A read that outlasts two heartbeat intervals means a silent peer.
		_ = c.SetReadDeadline(time.Now().Add(2*interval + 10*time.Second))
		var p payload
		if err := c.ReadJSON(&p); err != nil {
			select {
			case e := <-hbErr:
				return e
			default:
			}
			return err
		}
		switch p.Op {
		case opDispatch:
			if p.S > 0 {
				d.gwMu.Lock()
				d.seq = p.S
				d.gwMu.Unlock()
			}
			switch p.T {
			case "READY":
				var r struct {
					SessionID string `json:"session_id"`
					User      struct {
						ID       string `json:"id"`
						Username string `json:"username"`
					} `json:"user"`
				}
				_ = json.Unmarshal(p.D, &r)
				d.gwMu.Lock()
				d.sessionID, d.botID = r.SessionID, r.User.ID
				d.gwMu.Unlock()
				d.log.Info("qq gateway ready", "bot", r.User.Username, "intents", d.intents())
			case "RESUMED":
				d.log.Info("qq gateway session resumed")
			default:
				d.handleEvent(p.T, p.ID, p.D)
			}
		case opHeartbeat:
			// The server may ask for a beat out of turn.
			if err := beat(); err != nil {
				return err
			}
		case opHeartbeatAck:
			acked.Lock()
			pending = false
			acked.Unlock()
		case opReconnect:
			return errReconnect
		case opInvalidSession:
			return errInvalidSession
		}
	}
}
