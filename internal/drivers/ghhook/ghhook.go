// Package ghhook is a GitHub webhook intake driver. Unlike the chat webhook
// (which waits for a reply), a GitHub webhook expects a fast 200 and carries a
// signed JSON event, not a chat message. This driver verifies the
// X-Hub-Signature-256 HMAC, acknowledges immediately, and submits the event to
// the router as an agent-typed message whose payload is the decoded event. The
// route then delivers it to the owning project-manager session by repo slug.
//
// It attaches to the shared httpd.Server; the intake half only — there is
// nothing to reply to, so Deliver reports an error (events never carry a chat
// target).
package ghhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/httpd"
)

// Driver receives GitHub webhooks and forwards them as agent events.
type Driver struct {
	name   string
	path   string
	agent  string
	secret string // webhook secret for HMAC verification; empty = skip verify (dev)
	sink   router.Sink
	log    *slog.Logger
}

// New builds the driver and attaches its handler to the shared httpd.Server.
// secret is the GitHub webhook secret; when empty the signature check is
// skipped (local development only — never in production).
func New(name, path, agent, secret string, sink router.Sink, srv *httpd.Server, log *slog.Logger) *Driver {
	if path == "" {
		path = "/hooks/github/"
	}
	d := &Driver{
		name:   name,
		path:   path,
		agent:  agent,
		secret: secret,
		sink:   sink,
		log:    log.With("driver", "ghhook", "channel", name),
	}
	srv.Handle(path, d.handle)
	return d
}

func (d *Driver) Name() string { return d.name }
func (d *Driver) Path() string { return d.path }

// Deliver implements gateway.Driver. Events have no chat target, so there is
// nothing to deliver; this is always an error.
func (d *Driver) Deliver(replyTo string, text string, attachments []media.Part) error {
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
	d.log.Debug("ghhook event received", "event", event, "delivery", r.Header.Get("X-GitHub-Delivery"))

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

// Stop is a no-op now; the shared httpd.Server owns the listener lifetime. Kept
// as a method so callers that haven't been fully migrated don't break the build.
func (d *Driver) Stop(_ context.Context) {}
