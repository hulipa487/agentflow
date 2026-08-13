// Package webhook is the HTTP channel driver: POST {"from","text"} in,
// synchronous reply out. Inbound events go to the router; the session's
// reply resolves the pending request by correlation id. It attaches to the
// shared httpd.Server (see internal/drivers/httpd) rather than listening on
// its own port.
package webhook

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

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
	log   *slog.Logger

	seq     atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan string // request id → reply waiter
}

func New(name, path, agent string, sink router.Sink, srv *httpd.Server, log *slog.Logger) *Driver {
	if path == "" {
		path = "/webhook/"
	}
	d := &Driver{
		name:    name,
		path:    path,
		agent:   agent,
		sink:    sink,
		log:     log.With("driver", "webhook", "channel", name),
		pending: map[string]chan string{},
	}
	srv.Handle(path, d.handle)
	return d
}

func (d *Driver) Name() string { return d.name }
func (d *Driver) Path() string  { return d.path }

type inbound struct {
	From string `json:"from"`
	Text string `json:"text"`
}

func (d *Driver) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in inbound
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
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
			ID:      id,
			Type:    "user",
			From:    "user:webhook:" + in.From,
			Text:    in.Text,
			Channel: d.name,
			ReplyTo: id,
			Ts:      time.Now().Unix(),
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

// Deliver implements gateway.Driver: resolves the reply for the pending
// request this message belongs to.
func (d *Driver) Deliver(replyToID string, text string) error {
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