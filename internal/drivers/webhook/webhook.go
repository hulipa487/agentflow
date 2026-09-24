// Package webhook is the HTTP channel driver: POST {"from","text"} in,
// reply out. Inbound events go to the router; the session's reply resolves
// the pending request by correlation id. It attaches to the shared
// httpd.Server (see internal/drivers/httpd) rather than listening on its own
// port.
//
// Reply modes (channel config):
//   - sync (default): the POST parks until the agent replies or `timeout`
//     (default 55s) elapses → 504. The reply body is plain text.
//   - async (`async: true`): the POST returns 202 + {"id": ...} immediately;
//     the reply is collected via GET <path>result/<id> (200 + text when done,
//     202 while pending) or POSTed to a caller-supplied `callback_url` in the
//     request body. Async jobs are never cut off by the sync timeout — that
//     is the point of the mode — and expire after jobRetention.
//
// Inbound media: an optional "attachments" array of {mime, name, data
// (base64)} objects is accepted when the channel media policy allows the
// mime; parts land in the blob store and ride the message as descriptors.
package webhook

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/httpd"
)

// defaultTimeout is the sync reply wait when the channel sets no timeout.
const defaultTimeout = 55 * time.Second

// jobRetention bounds how long a finished (or never-polled) async job is kept.
const jobRetention = 15 * time.Minute

// Options tunes a webhook driver (from the channel's YAML block).
type Options struct {
	Timeout time.Duration // sync reply wait; <= 0 selects defaultTimeout
	Async   bool          // 202 + job id instead of parking the response
}

// Driver accepts webhooks and implements gateway.Driver for replies.
type Driver struct {
	name  string
	path  string
	agent string
	sink  router.Sink
	store media.Store // nil = media policy disabled
	pol   media.Policy
	opts  Options
	log   *slog.Logger
	http  *http.Client // callback delivery, guarded by the shared outbound policy

	seq     atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan string // request id → reply waiter (sync)
	jobs    map[string]*job        // request id → async result slot
}

// job is one async request's result slot. The reply lands here whenever the
// agent produces it; a poll reads it (consuming the job) and a callback_url
// fires once on completion.
type job struct {
	done     bool
	text     string
	callback string
	expires  time.Time
}

func New(name, path, agent string, sink router.Sink, srv *httpd.Server, store media.Store, pol media.Policy, opts Options, netPol netguard.Policy, log *slog.Logger) *Driver {
	if path == "" {
		path = "/webhook/"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	d := &Driver{
		name:    name,
		path:    path,
		agent:   agent,
		sink:    sink,
		store:   store,
		pol:     pol,
		opts:    opts,
		log:     log.With("driver", "webhook", "channel", name),
		// The callback client carries the same address guard the loop's
		// http.request and builtin:fetch use. callback_url is caller-supplied
		// and only scheme-checked, so without this any caller reaching the
		// channel could have the runtime POST an agent's reply to an internal
		// address — a service on the private network, or a cloud metadata
		// endpoint on link-local. Sharing one Policy also means
		// net.http.allow_private still permits a legitimate local receiver.
		http:    &http.Client{Timeout: 10 * time.Second, Transport: netPol.Transport()},
		pending: map[string]chan string{},
		jobs:    map[string]*job{},
	}
	srv.Handle(path, d.handle)
	return d
}

func (d *Driver) Name() string { return d.name }

type inbound struct {
	From        string          `json:"from"`
	Text        string          `json:"text"`
	Attachments []inboundAttach `json:"attachments"`
	CallbackURL string          `json:"callback_url"` // async mode: POST the reply here
}

type inboundAttach struct {
	MIME string `json:"mime"`
	Name string `json:"name"`
	Data string `json:"data"` // base64; the only supported webhook source
}

// maxRequestBytes bounds the whole POST body (text + inline base64 media).
const maxRequestBytes = 24 << 20 // 24 MiB

func (d *Driver) handle(w http.ResponseWriter, r *http.Request) {
	// Async result polling lives under the same mounted path.
	if r.Method == http.MethodGet {
		if id, ok := strings.CutPrefix(strings.TrimPrefix(r.URL.Path, d.path), "result/"); ok && id != "" {
			d.handleResult(w, id)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in inbound
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBytes)).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	d.log.Debug("webhook received", "from", in.From, "text_len", len(in.Text), "attachments", len(in.Attachments))

	id := fmt.Sprintf("wh-%d", d.seq.Add(1))

	if d.opts.Async {
		if in.CallbackURL != "" && !validCallback(in.CallbackURL) {
			http.Error(w, "callback_url must be http(s)", http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.sweepLocked()
		d.jobs[id] = &job{callback: in.CallbackURL, expires: time.Now().Add(jobRetention)}
		d.mu.Unlock()
		d.submit(id, in)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"id":%q,"status":"pending"}`, id)
		return
	}

	replyCh := make(chan string, 1)
	d.mu.Lock()
	d.pending[id] = replyCh
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pending, id)
		d.mu.Unlock()
	}()

	d.submit(id, in)

	select {
	case reply := <-replyCh:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, reply)
	case <-time.After(d.opts.Timeout):
		http.Error(w, "agent timeout", http.StatusGatewayTimeout)
	case <-r.Context().Done():
	}
}

// submit forwards the inbound event to the router with the correlation id.
func (d *Driver) submit(id string, in inbound) {
	d.sink.Submit(router.Inbound{
		Channel: d.name,
		Agent:   d.agent,
		Message: session.Message{
			ID:          id,
			Type:        "user",
			From:        "user:webhook:" + in.From,
			Text:        in.Text,
			Channel:     d.name,
			ReplyTo:     id,
			Attachments: d.ingestAttachments(in.Attachments),
			Ts:          time.Now().Unix(),
		},
	})
}

// handleResult serves GET <path>result/<id>: 200 + the reply when the job is
// done (consuming it), 202 while pending, 404 for an unknown id.
func (d *Driver) handleResult(w http.ResponseWriter, id string) {
	d.mu.Lock()
	j, ok := d.jobs[id]
	if ok && j.done {
		delete(d.jobs, id)
	}
	d.mu.Unlock()
	if !ok {
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}
	if !j.done {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"id":%q,"status":"pending"}`, id)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, j.text)
}

// validCallback restricts callback URLs to plain http(s) so a webhook caller
// cannot turn the runtime into a file:/gopher: client.
func validCallback(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// sweepLocked drops expired async jobs. Caller holds d.mu.
func (d *Driver) sweepLocked() {
	now := time.Now()
	for id, j := range d.jobs {
		if now.After(j.expires) {
			delete(d.jobs, id)
		}
	}
}

// ingestAttachments stores inline base64 attachments per the channel media
// policy. Parts that fail the policy or decode are skipped with a log line —
// the text still flows (honest degradation at the channel boundary).
func (d *Driver) ingestAttachments(in []inboundAttach) []media.Part {
	if len(in) == 0 || d.store == nil || !d.pol.Enabled() {
		return nil
	}
	var out []media.Part
	for _, a := range in {
		mime := a.MIME
		if mime == "" {
			mime = "application/octet-stream"
		}
		if !d.pol.Allows(mime) {
			d.log.Warn("webhook attachment dropped: mime not allowed", "mime", mime)
			metrics.Inc("agentflow_media_unsupported")
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			d.log.Warn("webhook attachment dropped: bad base64", "name", a.Name)
			metrics.Inc("agentflow_media_unsupported")
			continue
		}
		ref, err := d.store.Put(bytes.NewReader(raw), mime, d.pol)
		if err != nil {
			d.log.Warn("webhook attachment store failed", "err", err)
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

// Deliver implements gateway.Driver: resolves the reply for the pending
// request this message belongs to. Media replies are not supported on this
// channel (the response body is text) — they fail loudly, never silently.
func (d *Driver) Deliver(replyToID string, text string, attachments []media.Part) error {
	if len(attachments) > 0 {
		return fmt.Errorf("webhook channel %q does not support media replies", d.name)
	}
	d.mu.Lock()
	if ch, ok := d.pending[replyToID]; ok {
		delete(d.pending, replyToID)
		d.mu.Unlock()
		select {
		case ch <- text:
		default:
		}
		return nil
	}
	if j, ok := d.jobs[replyToID]; ok {
		j.done = true
		j.text = text
		j.expires = time.Now().Add(jobRetention)
		callback := j.callback
		d.mu.Unlock()
		if callback != "" {
			go d.postCallback(callback, text)
		}
		return nil
	}
	d.mu.Unlock()
	return fmt.Errorf("no pending request %q", replyToID)
}

// postCallback delivers an async reply to the caller-supplied URL. Failures
// are logged, never fatal — the result stays pollable until expiry.
func (d *Driver) postCallback(url, text string) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(text))
	if err != nil {
		d.log.Warn("webhook callback build failed", "err", err)
		return
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := d.http.Do(req)
	if err != nil {
		d.log.Warn("webhook callback failed", "url", url, "err", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		d.log.Warn("webhook callback rejected", "url", url, "status", resp.StatusCode)
	}
}
