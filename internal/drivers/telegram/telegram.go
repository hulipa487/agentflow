// Package telegram is the Telegram channel driver.
//
// Three modes, selected by the "mode" channel config:
//   - "polling" (default): long-poll getUpdates in, sendMessage out.
//   - "webhook": receives POST updates at a path, sendMessage out. Requires
//     public_url to be set (the runtime must be externally reachable).
//   - "auto": health-probe the public_url at startup — setWebhook if
//     reachable, otherwise deleteWebhook and long-poll. Falls back to polling
//     without the webhook host dying silently.
//
// Access control (allow_users) is enforced in all modes before any Lua runs.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/httpd"
)

// Driver is the Telegram channel driver. It implements gateway.Driver for
// replies (sendMessage) and either polls or serves a webhook for inbound.
type Driver struct {
	name      string
	token     string
	agent     string
	allow     map[int64]bool // empty = allow all
	mode      string          // "polling" | "webhook" | "auto"
	path      string          // webhook mode only
	publicURL string          // webhook/auto: external base that routes to this process
	apiBase   string          // override for the Telegram API host (tests); empty = api.telegram.org
	sink      router.Sink
	log       *slog.Logger
	client    *http.Client
	seq       int64
}

func New(name, token, agent, mode string, allowUsers []int64, path, publicURL string, sink router.Sink, srv *httpd.Server, log *slog.Logger) *Driver {
	allow := map[int64]bool{}
	for _, id := range allowUsers {
		allow[id] = true
	}
	if mode == "" {
		mode = "polling"
	}
	d := &Driver{
		name:      name,
		token:     token,
		agent:     agent,
		allow:     allow,
		mode:      mode,
		path:      path,
		publicURL: publicURL,
		sink:      sink,
		log:       log.With("driver", "telegram", "channel", name, "mode", mode),
		client:    &http.Client{Timeout: 40 * time.Second},
	}
	// webhook and auto both serve the webhook path, so both attach their handler
	// up front. auto may later fall back to polling, but the mounted path is
	// then simply unused (Telegram won't call it after deleteWebhook) — harmless,
	// and it lets the listener bind before the probe runs.
	if mode == "webhook" || mode == "auto" {
		if d.path == "" {
			d.path = "/webhook/telegram/"
		}
		srv.Handle(d.path, d.handleWebhook)
	}
	return d
}

func (d *Driver) Name() string { return d.name }

// Start launches the driver by mode. It must be called AFTER the shared
// httpd.Server has bound its listener (see main.go), because auto probes
// <public_url>/health — which round-trips through the public proxy back to that
// listener — and a probe before bind would always fail.
func (d *Driver) Start(ctx context.Context) error {
	switch d.mode {
	case "polling":
		go d.poll(ctx)
		return nil
	case "webhook":
		if d.publicURL == "" {
			return fmt.Errorf("telegram webhook mode requires gateway.public_url")
		}
		if err := d.setWebhook(d.publicURL + strings.TrimRight(d.path, "/") + "/"); err != nil {
			return fmt.Errorf("setWebhook: %w", err)
		}
		d.log.Info("telegram webhook registered", "url", d.publicURL+d.path)
		return nil
	case "auto":
		return d.startAuto(ctx)
	default:
		return fmt.Errorf("telegram: unknown mode %q (use polling, webhook, or auto)", d.mode)
	}
}

// startAuto implements the health-gated webhook→polling fallback.
func (d *Driver) startAuto(ctx context.Context) error {
	if d.publicURL == "" {
		d.log.Info("auto: no public_url, polling")
		go d.poll(ctx)
		return nil
	}
	r := httpd.Probe(ctx, d.publicURL)
	d.log.Info("auto: health probe", "url", d.publicURL, "ok", r.OK, "latency_ms", r.Latency.Milliseconds(), "err", r.Err)
	if !r.OK {
		// Webhook host is unreachable: ensure Telegram isn't holding a stale
		// webhook URL against us, then long-poll. This is the documented
		// fallback — a failed GET to the domain disables webhook.
		if err := d.deleteWebhook(); err != nil {
			d.log.Warn("auto: deleteWebhook failed", "err", err)
		}
		d.log.Info("auto: webhook unreachable, falling back to polling")
		go d.poll(ctx)
		return nil
	}
	// healthy: register the webhook with Telegram. The handler is already
	// mounted (New attaches it for auto too), so no registration here.
	hookURL := d.publicURL + strings.TrimRight(d.path, "/") + "/"
	if err := d.setWebhook(hookURL); err != nil {
		d.log.Warn("auto: setWebhook failed, polling instead", "err", err)
		go d.poll(ctx)
		return nil
	}
	d.log.Info("auto: webhook registered", "url", hookURL)
	return nil
}

// Stop is a no-op now; the shared httpd.Server owns the listener lifetime and
// polling exits via ctx cancellation. Method retained for the Driver surface.
func (d *Driver) Stop(_ context.Context) {}

func (d *Driver) apiURL(method string) string {
	base := d.apiBase
	if base == "" {
		base = "https://api.telegram.org"
	}
	return base + "/bot" + d.token + "/" + method
}

// setWebhook registers hookURL with Telegram so updates are pushed there.
// An empty URL clears the webhook (see deleteWebhook). drain is false so we
// don't drop in-flight updates on the switch.
func (d *Driver) setWebhook(hookURL string) error {
	body, _ := json.Marshal(map[string]any{
		"url":             hookURL,
		"allowed_updates": []string{"message"},
	})
	return d.postAPI("setWebhook", body)
}

// deleteWebhook clears any registered webhook URL so Telegram stops pushing.
func (d *Driver) deleteWebhook() error {
	body, _ := json.Marshal(map[string]any{"drop_pending_updates": false})
	return d.postAPI("deleteWebhook", body)
}

func (d *Driver) postAPI(method string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.apiURL(method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %d: %s", method, resp.StatusCode, b)
	}
	return nil
}

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64 `json:"message_id"`
		Text      string `json:"text"`
		From      struct {
			ID        int64  `json:"id"`
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
		} `json:"from"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
	} `json:"message"`
}

type updatesResponse struct {
	OK          bool     `json:"ok"`
	Result      []update `json:"result"`
	Description string   `json:"description"`
}

// --- polling mode ---

func (d *Driver) poll(ctx context.Context) {
	d.log.Info("telegram polling started")
	var offset int64
	for ctx.Err() == nil {
		updates, err := d.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.log.Warn("getUpdates failed", "err", err)
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			d.handleUpdate(u)
		}
	}
}

func (d *Driver) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	url := d.apiURL("getUpdates") +
		"?timeout=30&offset=" + strconv.FormatInt(offset, 10) +
		`&allowed_updates=["message"]`
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var ur updatesResponse
	if err := json.Unmarshal(body, &ur); err != nil {
		return nil, fmt.Errorf("bad getUpdates response: %w", err)
	}
	if !ur.OK {
		return nil, fmt.Errorf("telegram api: %s", ur.Description)
	}
	return ur.Result, nil
}

// --- webhook mode ---

func (d *Driver) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var u update
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&u); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	d.handleUpdate(u)
	w.WriteHeader(http.StatusOK)
}

// --- shared ---

func (d *Driver) handleUpdate(u update) {
	m := u.Message
	if m == nil || m.Text == "" {
		return
	}
	if len(d.allow) > 0 && !d.allow[m.From.ID] {
		d.log.Warn("dropping message from non-allowed user", "user_id", m.From.ID)
		return
	}
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	d.seq++
	d.sink.Submit(router.Inbound{
		Channel: d.name,
		Agent:   d.agent,
		Message: session.Message{
			ID:      fmt.Sprintf("tg-%d", u.UpdateID),
			Type:    "user",
			From:    "user:telegram:" + strconv.FormatInt(m.From.ID, 10),
			Text:    m.Text,
			Channel: d.name,
			ReplyTo: chatID,
			Ts:      time.Now().Unix(),
			Payload: map[string]any{
				"chat_id":  chatID,
				"user_id":  m.From.ID,
				"username": m.From.Username,
				"name":     m.From.FirstName,
			},
		},
	})
}

// Deliver implements gateway.Driver: sendMessage to the chat.
func (d *Driver) Deliver(replyTo, text string) error {
	body, _ := json.Marshal(map[string]any{
		"chat_id": replyTo,
		"text":    text,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.apiURL("sendMessage"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("sendMessage: %d: %s", resp.StatusCode, b)
	}
	return nil
}