// Package webhook is the HTTP channel driver: POST {"from","text"} in,
// synchronous reply out. Inbound events go to the router; the session's
// reply resolves the pending request by correlation id. It attaches to the
// shared httpd.Server (see internal/drivers/httpd) rather than listening on
// its own port.
//
// Inbound media: an optional "attachments" array of {mime, name, data
// (base64)} objects is accepted when the channel media policy allows the
// mime; parts land in the blob store and ride the message as descriptors.
package webhook

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/httpd"
)

// Driver accepts webhooks and implements gateway.Driver for replies.
type Driver struct {
	name  string
	path  string
	agent string
	sink  router.Sink
	store *media.Store // nil = media policy disabled
	pol   media.Policy
	log   *slog.Logger

	seq     atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan string // request id → reply waiter
}

func New(name, path, agent string, sink router.Sink, srv *httpd.Server, store *media.Store, pol media.Policy, log *slog.Logger) *Driver {
	if path == "" {
		path = "/webhook/"
	}
	d := &Driver{
		name:    name,
		path:    path,
		agent:   agent,
		sink:    sink,
		store:   store,
		pol:     pol,
		log:     log.With("driver", "webhook", "channel", name),
		pending: map[string]chan string{},
	}
	srv.Handle(path, d.handle)
	return d
}

func (d *Driver) Name() string { return d.name }
func (d *Driver) Path() string { return d.path }

type inbound struct {
	From        string          `json:"from"`
	Text        string          `json:"text"`
	Attachments []inboundAttach `json:"attachments"`
}

type inboundAttach struct {
	MIME string `json:"mime"`
	Name string `json:"name"`
	Data string `json:"data"` // base64; the only supported webhook source
}

// maxRequestBytes bounds the whole POST body (text + inline base64 media).
const maxRequestBytes = 24 << 20 // 24 MiB

func (d *Driver) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in inbound
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBytes)).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	id := fmt.Sprintf("wh-%d", d.seq.Add(1))
	replyCh := make(chan string, 1)
	d.mu.Lock()
	d.pending[id] = replyCh
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pending, id)
		d.mu.Unlock()
	}()

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

	select {
	case reply := <-replyCh:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, reply)
	case <-time.After(55 * time.Second):
		http.Error(w, "agent timeout", http.StatusGatewayTimeout)
	case <-r.Context().Done():
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
	ch, ok := d.pending[replyToID]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending request %q", replyToID)
	}
	select {
	case ch <- text:
	default:
	}
	return nil
}
