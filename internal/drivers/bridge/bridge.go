// Package bridge is the out-of-process channel adapter. One bridge driver
// covers any chat platform: an external process (in any language) verifies the
// platform's signatures, translates events into this protocol, and receives
// outbound deliveries. Adding Slack, Discord, Feishu, Teams, or a bespoke
// platform therefore does not require a Go change in this repository.
//
// Inbound (adapter -> runtime):
//
//	POST <path>
//	X-Agentflow-Signature: sha256=<hex HMAC-SHA256 of the raw body>
//	{"from":"u123","chat_id":"c456","text":"hi","attachments":[{"mime":"image/png","name":"x.png","data":"<base64>"}]}
//
// Outbound (runtime -> adapter):
//
//	POST <deliver_url>
//	{"to":"c456","text":"...","attachments":[{"type":"image","mime":"image/png","handle":"media:..."}]}
//
// The engine treats the adapter as trusted: the HMAC is what makes an inbound
// event attributable to it. It does not make the *end user* verified, so the
// identity layer records bridge handles as asserted and unlinkable by default.
package bridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/httpd"
)

const maxRequestBytes = 24 << 20

// Options configures one bridge channel.
type Options struct {
	Path       string
	DeliverURL string
	Secret     string // HMAC key for inbound requests; required
	Token      string // optional bearer token for outbound delivery
	Timeout    time.Duration
}

// Driver accepts bridge events and posts replies to the adapter.
type Driver struct {
	name  string
	agent string
	opts  Options
	sink  router.Sink
	store media.Store
	media *media.Service
	pol   media.Policy
	http  *http.Client
	log   *slog.Logger
}

// New mounts the bridge's inbound route on the shared server and returns the
// driver for outbound delivery.
func New(name, agent string, opts Options, sink router.Sink, srv *httpd.Server, store media.Store, pol media.Policy, log *slog.Logger) *Driver {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	d := &Driver{
		name:  name,
		agent: agent,
		opts:  opts,
		sink:  sink,
		store: store,
		pol:   pol,
		http:  &http.Client{Timeout: opts.Timeout},
		log:   log.With("driver", "bridge", "channel", name),
	}
	srv.Handle(opts.Path, d.handle)
	return d
}

func (d *Driver) Name() string { return d.name }

// SetMedia attaches the centralized media service so handle-only outbound
// attachments are resolved into presigned URLs (when available) or base64 data
// before crossing the HTTP boundary to an out-of-process bridge adapter.
func (d *Driver) SetMedia(svc *media.Service) { d.media = svc }

type inbound struct {
	From        string       `json:"from"`
	ChatID      string       `json:"chat_id"`
	ReplyTo     string       `json:"reply_to"`
	Text        string       `json:"text"`
	Attachments []attachment `json:"attachments"`
}

type attachment struct {
	MIME string `json:"mime"`
	Name string `json:"name"`
	Data string `json:"data"` // base64
}

type outbound struct {
	To          string       `json:"to"`
	Text        string       `json:"text"`
	Attachments []media.Part `json:"attachments,omitempty"`
}

func (d *Driver) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxRequestBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !d.verify(body, r.Header.Get("X-Agentflow-Signature")) {
		metrics.Inc("agentflow_bridge_auth_failed")
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var in inbound
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	to := in.ChatID
	if to == "" {
		to = in.ReplyTo
	}
	if to == "" {
		http.Error(w, "chat_id or reply_to is required", http.StatusBadRequest)
		return
	}
	from := in.From
	if from == "" {
		from = to
	}
	d.sink.Submit(router.Inbound{
		Channel: d.name,
		Agent:   d.agent,
		Message: session.Message{
			ID:          fmt.Sprintf("bridge-%d", time.Now().UnixNano()),
			Type:        "user",
			From:        "user:bridge:" + from,
			Text:        in.Text,
			Channel:     d.name,
			ReplyTo:     to,
			Attachments: d.ingest(in.Attachments),
			Ts:          time.Now().Unix(),
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (d *Driver) verify(body []byte, header string) bool {
	if d.opts.Secret == "" {
		return false
	}
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(d.opts.Secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func (d *Driver) ingest(in []attachment) []media.Part {
	if len(in) == 0 || d.store == nil || !d.pol.Enabled() {
		return nil
	}
	out := make([]media.Part, 0, len(in))
	for _, a := range in {
		mime := a.MIME
		if mime == "" {
			mime = "application/octet-stream"
		}
		if !d.pol.Allows(mime) {
			d.log.Warn("bridge attachment dropped: mime not allowed", "mime", mime)
			metrics.Inc("agentflow_media_unsupported")
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			d.log.Warn("bridge attachment dropped: bad base64", "name", a.Name)
			metrics.Inc("agentflow_media_unsupported")
			continue
		}
		ref, err := d.store.Put(bytes.NewReader(raw), mime, d.pol)
		if err != nil {
			d.log.Warn("bridge attachment store failed", "err", err)
			continue
		}
		metrics.Add("agentflow_media_bytes", ref.Size)
		out = append(out, media.Part{Type: media.Classify(mime), MIME: mime, Handle: ref.Handle, Name: a.Name})
	}
	if len(out) > 0 {
		metrics.Inc("agentflow_media_ingested")
	}
	return out
}

// Deliver posts the reply to the adapter. The adapter owns the platform call;
// this driver only knows the neutral outbound shape. When a centralized
// media.Service is wired, internal handles are materialized to presigned URLs
// (preferred) or inline base64 so external bridge adapters never see opaque
// internal handles they cannot read.
func (d *Driver) Deliver(replyTo, text string, attachments []media.Part) error {
	resolved := attachments
	if d.media != nil && len(attachments) > 0 {
		resolved = make([]media.Part, len(attachments))
		ctx, cancel := context.WithTimeout(context.Background(), d.opts.Timeout)
		defer cancel()
		for i, att := range attachments {
			if att.Handle != "" && att.URL == "" && att.Data == "" {
				if d.media.CanPresign() {
					link, err := d.media.MaterializeURL(ctx, media.ScopeMaintenance, att, media.TargetSpec{AttachFilename: att.Type == "file"})
					if err != nil {
						return fmt.Errorf("bridge %q: presign attachment: %w", d.name, err)
					}
					att.URL = link
					att.Handle = ""
				} else {
					blob, err := d.media.MaterializeBytes(ctx, media.ScopeMaintenance, att, media.TargetSpec{MaxBytes: 50 << 20})
					if err != nil {
						return fmt.Errorf("bridge %q: materialize attachment: %w", d.name, err)
					}
					att.Data = base64.StdEncoding.EncodeToString(blob.Bytes)
					if blob.MIME != "" {
						att.MIME = blob.MIME
					}
					att.Handle = ""
				}
			}
			resolved[i] = att
		}
	}
	payload := outbound{To: replyTo, Text: text, Attachments: resolved}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("bridge %q: marshal delivery: %w", d.name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.opts.DeliverURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("bridge %q: build delivery: %w", d.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if d.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+d.opts.Token)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("bridge %q: deliver: %w", d.name, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("bridge %q: adapter rejected delivery: %s", d.name, resp.Status)
	}
	return nil
}
