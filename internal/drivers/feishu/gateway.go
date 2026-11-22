package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultPingInterval = 120 * time.Second
	maxChunkCacheAge    = 30 * time.Second
)

type wsEndpointResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		URL          string `json:"URL"`
		ClientConfig struct {
			PingInterval int `json:"PingInterval"`
		} `json:"ClientConfig"`
	} `json:"data"`
}

// runGateway keeps a Feishu persistent WebSocket connection open until ctx
// is canceled, reconnecting with exponential backoff on drops.
func (d *Driver) runGateway(ctx context.Context) {
	delay := d.retryBase
	for ctx.Err() == nil {
		started := time.Now()
		err := d.connectWS(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > 30*time.Second {
			delay = d.retryBase
		}
		jitter := time.Duration(rand.Int64N(int64(delay/4 + 1)))
		wait := delay + jitter
		d.log.Warn("feishu websocket disconnected; reconnecting", "err", err, "retry_in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		delay = min(delay*2, 60*time.Second)
	}
}

func (d *Driver) fetchWSEndpoint(ctx context.Context) (string, time.Duration, error) {
	if d.wsURL != "" {
		return d.wsURL, defaultPingInterval, nil
	}
	body, _ := json.Marshal(map[string]string{
		"AppID":     d.appID,
		"AppSecret": d.appSecret,
	})
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, d.apiBase+"/callback/ws/endpoint", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("locale", "zh")
	resp, err := d.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("feishu ws endpoint: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", 0, fmt.Errorf("feishu ws endpoint read: %w", err)
	}
	var out wsEndpointResp
	if err := json.Unmarshal(data, &out); err != nil {
		return "", 0, fmt.Errorf("feishu ws endpoint status %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || out.Code != 0 || out.Data.URL == "" {
		return "", 0, fmt.Errorf("feishu ws endpoint refused (status %d, code %d): %s", resp.StatusCode, out.Code, out.Msg)
	}
	ping := defaultPingInterval
	if out.Data.ClientConfig.PingInterval > 0 {
		ping = time.Duration(out.Data.ClientConfig.PingInterval) * time.Second
	}
	return out.Data.URL, ping, nil
}

func parseServiceID(rawURL string) int32 {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(u.Query().Get("service_id"), 10, 32)
	return int32(v)
}

type chunkBuf struct {
	parts     [][]byte
	received  int
	createdAt time.Time
}

func (d *Driver) connectWS(ctx context.Context) error {
	wsURL, pingInterval, err := d.fetchWSEndpoint(ctx)
	if err != nil {
		return err
	}
	serviceID := parseServiceID(wsURL)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("feishu ws dial: %w", err)
	}
	defer conn.Close()

	d.log.Info("feishu websocket connected", "service_id", serviceID, "ping_interval", pingInterval)

	// Cancel-on-ctx watcher closes the socket cleanly on shutdown.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	var writeMu sync.Mutex
	sendFrame := func(f pbbp2Frame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(websocket.BinaryMessage, f.marshal())
	}

	pingIntervalCh := make(chan time.Duration, 1)
	go func() {
		interval := pingInterval
		t := time.NewTicker(interval)
		defer t.Stop()
		// Send initial ping immediately so servers learn our keepalive early.
		sendPing := func() bool {
			err := sendFrame(pbbp2Frame{
				Service: serviceID,
				Method:  frameMethodControl,
				Headers: []pbbp2Header{{Key: "type", Value: "ping"}},
			})
			return err == nil
		}
		if !sendPing() {
			return
		}
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case next := <-pingIntervalCh:
				if next > 0 && next != interval {
					interval = next
					t.Reset(interval)
				}
			case <-t.C:
				if !sendPing() {
					_ = conn.Close()
					return
				}
			}
		}
	}()

	chunks := map[string]*chunkBuf{}
	for {
		msgType, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if msgType != websocket.BinaryMessage {
			continue
		}
		frame, err := unmarshalFrame(raw)
		if err != nil {
			d.log.Warn("feishu ws bad pbbp2 frame", "err", err)
			continue
		}
		switch frame.Method {
		case frameMethodControl:
			if frame.header("type") == "pong" && len(frame.Payload) > 0 {
				var cfg struct {
					PingInterval int `json:"PingInterval"`
				}
				if json.Unmarshal(frame.Payload, &cfg) == nil && cfg.PingInterval > 0 {
					select {
					case pingIntervalCh <- time.Duration(cfg.PingInterval) * time.Second:
					default:
					}
				}
			}
		case frameMethodData:
			payload := assemblePayload(chunks, &frame)
			if payload != nil {
				// Acknowledge immediately on the socket before waiting on routing/LLM,
				// because Feishu retries if ACK takes > 3 seconds.
				start := time.Now()
				headers := append([]pbbp2Header(nil), frame.Headers...)
				headers = append(headers, pbbp2Header{Key: "biz_rt", Value: strconv.FormatInt(time.Since(start).Milliseconds(), 10)})
				ack := pbbp2Frame{
					SeqID:           frame.SeqID,
					LogID:           frame.LogID,
					Service:         frame.Service,
					Method:          frame.Method,
					Headers:         headers,
					PayloadEncoding: frame.PayloadEncoding,
					PayloadType:     frame.PayloadType,
					Payload:         []byte(`{"code":200}`),
					LogIDNew:        frame.LogIDNew,
				}
				if err := sendFrame(ack); err != nil {
					return err
				}
				if frame.header("type") == "event" || frame.header("type") == "" {
					go d.handleEventJSON(payload)
				}
			}
		}
	}
}

func assemblePayload(chunks map[string]*chunkBuf, f *pbbp2Frame) []byte {
	sumStr := f.header("sum")
	if sumStr == "" {
		return f.Payload
	}
	sum, err := strconv.Atoi(sumStr)
	if err != nil || sum <= 1 {
		return f.Payload
	}
	seq, err := strconv.Atoi(f.header("seq"))
	msgID := f.header("message_id")
	if err != nil || msgID == "" || seq < 0 || seq >= sum {
		return nil
	}
	now := time.Now()
	for id, buf := range chunks {
		if now.Sub(buf.createdAt) > maxChunkCacheAge {
			delete(chunks, id)
		}
	}
	buf := chunks[msgID]
	if buf == nil || len(buf.parts) != sum {
		buf = &chunkBuf{parts: make([][]byte, sum), createdAt: now}
		chunks[msgID] = buf
	}
	if buf.parts[seq] == nil {
		buf.received++
	}
	buf.parts[seq] = append([]byte(nil), f.Payload...)
	if buf.received < sum {
		return nil
	}
	delete(chunks, msgID)
	total := 0
	for _, p := range buf.parts {
		total += len(p)
	}
	out := make([]byte, 0, total)
	for _, p := range buf.parts {
		out = append(out, p...)
	}
	return out
}
