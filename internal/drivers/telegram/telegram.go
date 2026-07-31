// Package telegram is the Telegram channel driver.
//
// Two modes are supported, selected by the "mode" channel config:
//   - "polling" (default): long-poll getUpdates in, sendMessage out.
//   - "webhook": receives POST updates at a path, sendMessage out.
//
// Access control (allow_users) is enforced in both modes before any Lua runs.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
)

// Driver is the Telegram channel driver. It implements gateway.Driver for
// replies (sendMessage) and either polls or serves a webhook for inbound.
type Driver struct {
	name    string
	token   string
	agent   string
	allow   map[int64]bool // empty = allow all
	mode    string          // "polling" | "webhook"
	listen  string          // webhook mode only
	path    string          // webhook mode only
	sink    router.Sink
	log     *slog.Logger
	client  *http.Client
	seq     int64
	srv     *http.Server
}

func New(name, token, agent, mode string, allowUsers []int64, listen, path string, sink router.Sink, log *slog.Logger) *Driver {
	allow := map[int64]bool{}
	for _, id := range allowUsers {
		allow[id] = true
	}
	if mode == "" {
		mode = "polling"
	}
	return &Driver{
		name:    name,
		token:   token,
		agent:   agent,
		allow:   allow,
		mode:    mode,
		listen:  listen,
		path:     path,
		sink:    sink,
		log:     log.With("driver", "telegram", "channel", name, "mode", mode),
		client:  &http.Client{Timeout: 40 * time.Second},
	}
}

func (d *Driver) Name() string { return d.name }

// Start launches the driver. In polling mode, it runs a long-poll loop. In
// webhook mode, it starts an HTTP server receiving updates at the configured path.
func (d *Driver) Start(ctx context.Context) error {
	switch d.mode {
	case "polling":
		go d.poll(ctx)
		return nil
	case "webhook":
		return d.startWebhook(ctx)
	default:
		return fmt.Errorf("telegram: unknown mode %q (use polling or webhook)", d.mode)
	}
}

// Stop shuts down the webhook server if running.
func (d *Driver) Stop(ctx context.Context) {
	if d.srv != nil {
		_ = d.srv.Shutdown(ctx)
	}
}

func (d *Driver) apiURL(method string) string {
	return "https://api.telegram.org/bot" + d.token + "/" + method
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

func (d *Driver) startWebhook(ctx context.Context) error {
	if d.listen == "" {
		return fmt.Errorf("telegram webhook: listen is required")
	}
	if d.path == "" {
		d.path = "/telegram/" + d.name
	}
	mux := http.NewServeMux()
	mux.HandleFunc(d.path, d.handleWebhook)
	d.srv = &http.Server{Addr: d.listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	d.log.Info("telegram webhook listening", "addr", d.listen, "path", d.path)
	go func() {
		if err := d.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("telegram webhook server died", "err", err)
		}
	}()
	return nil
}

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
				"name":      m.From.FirstName,
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
