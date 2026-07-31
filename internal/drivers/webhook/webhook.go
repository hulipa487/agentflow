// Package webhook is the HTTP channel driver: POST {"from","text"} in,
// synchronous reply out. Inbound events go to the router; the session's
// reply resolves the pending request by correlation id.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
)

// Driver accepts webhooks and implements gateway.Driver for replies.
type Driver struct {
	name   string
	listen string
	path   string
	agent  string
	sink   router.Sink
	log    *slog.Logger

	seq     atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan string // request id → reply waiter
	srv     *http.Server
}

func New(name, listen, path, agent string, sink router.Sink, log *slog.Logger) *Driver {
	if path == "" {
		path = "/webhook"
	}
	return &Driver{
		name:    name,
		listen:  listen,
		path:    path,
		agent:   agent,
		sink:    sink,
		log:     log.With("driver", "webhook", "channel", name),
		pending: map[string]chan string{},
	}
}

func (d *Driver) Name() string { return d.name }

type inbound struct {
	From string `json:"from"`
	Text string `json:"text"`
}

func (d *Driver) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc(d.path, d.handle)
	d.srv = &http.Server{Addr: d.listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	d.log.Info("webhook listening", "addr", d.listen, "path", d.path)
	go func() {
		if err := d.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("webhook server died", "err", err)
		}
	}()
	return nil
}

func (d *Driver) Stop(ctx context.Context) {
	if d.srv != nil {
		_ = d.srv.Shutdown(ctx)
	}
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
