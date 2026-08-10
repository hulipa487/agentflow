// Package ghhook is a GitHub webhook intake driver. Unlike the chat webhook
// (which waits for a reply), a GitHub webhook expects a fast 200 and carries a
// signed JSON event, not a chat message. This driver verifies the
// X-Hub-Signature-256 HMAC, acknowledges immediately, and submits the event to
// the router as an agent-typed message whose payload is the decoded event. The
// route then delivers it to the owning project-manager session by repo slug.
//
// It implements the intake half only; there is nothing to reply to, so Deliver
// reports an error (events never carry a chat target).
package ghhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
)

// Driver receives GitHub webhooks and forwards them as agent events.
type Driver struct {
	name   string
	listen string
	path   string
	agent  string
	secret string // webhook secret for HMAC verification; empty = skip verify (dev)
	sink   router.Sink
	log    *slog.Logger

	srv *http.Server
}

// New builds the driver. secret is the GitHub webhook secret; when empty the
// signature check is skipped (local development only — never in production).
func New(name, listen, path, agent, secret string, sink router.Sink, log *slog.Logger) *Driver {
	if path == "" {
		path = "/hooks/github"
	}
	return &Driver{
		name:   name,
		listen: listen,
		path:   path,
		agent:  agent,
		secret: secret,
		sink:   sink,
		log:    log.With("driver", "ghhook", "channel", name),
	}
}

func (d *Driver) Name() string { return d.name }

// Deliver implements gateway.Driver. Events have no chat target, so there is
// nothing to deliver; this is always an error.
func (d *Driver) Deliver(replyTo, text string) error {
	return fmt.Errorf("ghhook channel %q does not support replies", d.name)
}

// verifySignature checks the X-Hub-Signature-256 header against the body.
func (d *Driver) verifySignature(body []byte, sigHeader string) bool {
	if d.secret == "" {
		return true
	}
	const prefix = "sha256="
	if len(sigHeader) <= len(prefix) || sigHeader[:len(prefix)] != prefix {
		return false
	}
	got, err := hex.DecodeString(sigHeader[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(d.secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func (d *Driver) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !d.verifySignature(body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	// Acknowledge immediately; the event is processed asynchronously downstream.
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")

	d.sink.Submit(router.Inbound{
		Channel: d.name,
		Agent:   d.agent,
		Message: session.Message{
			ID:      fmt.Sprintf("gh-%d", time.Now().UnixNano()),
			Type:    "agent",
			From:    "github",
			Channel: d.name,
			Ts:      time.Now().Unix(),
			Payload: map[string]any{
				"type":   "project_event",
				"source": "github",
				"event":  event,
				"data":   payload,
			},
		},
	})
}

func (d *Driver) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc(d.path, d.handle)
	d.srv = &http.Server{Addr: d.listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	d.log.Info("github webhook listening", "addr", d.listen, "path", d.path)
	go func() {
		if err := d.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("github webhook server died", "err", err)
		}
	}()
	return nil
}

func (d *Driver) Stop(ctx context.Context) {
	if d.srv != nil {
		_ = d.srv.Shutdown(ctx)
	}
}
