// Package request tracks parked agent.request calls. It deliberately holds
// only channels and opaque correlation IDs; the supervisor is responsible for
// authorizing and delivering the underlying messages.
package request

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"agentflow/internal/core/session"
)

// Registry owns request/reply waiters.
type Registry struct {
	mu      sync.Mutex
	pending map[string]pending
}

type pending struct {
	owner     string
	recipient string
	ch        chan session.Message
}

func New() *Registry { return &Registry{pending: map[string]pending{}} }

// Open creates a new runtime-generated correlation ID bound to the intended
// recipient agent. The caller must call cancel when delivery fails before
// waiting.
func (r *Registry) Open(owner, recipient string) (id string, wait <-chan session.Message, cancel func()) {
	id = "req-" + uuid.NewString()
	ch := make(chan session.Message, 1)
	r.mu.Lock()
	r.pending[id] = pending{owner: owner, recipient: recipient, ch: ch}
	r.mu.Unlock()
	return id, ch, func() { r.Cancel(id) }
}

// Wait blocks for a reply or context cancellation, removing the waiter in all
// cases. A reply whose correlation ID is unknown is intentionally ignored.
func (r *Registry) Wait(ctx context.Context, id string, wait <-chan session.Message) (session.Message, error) {
	defer r.Cancel(id)
	select {
	case msg := <-wait:
		return msg, nil
	case <-ctx.Done():
		return session.Message{}, ctx.Err()
	}
}

// Resolve delivers a reply exactly once, but only when it comes from the
// agent that received the original request. A leaked opaque request ID alone
// is not sufficient authority to resolve another agent's request.
func (r *Registry) Resolve(id, recipient string, msg session.Message) bool {
	r.mu.Lock()
	p, ok := r.pending[id]
	if ok && p.recipient == recipient {
		delete(r.pending, id)
	} else {
		ok = false
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	p.ch <- msg
	return true
}

func (r *Registry) Cancel(id string) {
	r.mu.Lock()
	delete(r.pending, id)
	r.mu.Unlock()
}

// CancelOwner removes every outstanding request opened by a session.
func (r *Registry) CancelOwner(owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.pending {
		if p.owner == owner {
			delete(r.pending, id)
		}
	}
}

func (r *Registry) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}
