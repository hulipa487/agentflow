// Package sessionhub decides where a session's messages are delivered.
//
// A session is a conversation — one (agent, key) — and in a fleet it runs on
// exactly one instance at a time. Every other instance may still receive
// traffic for it: a webhook load-balanced across the fleet, a trigger firing
// anywhere, a push from any node. The hub is the piece that makes those
// arrivals correct.
//
// The rule is a lease per session:
//
//   - The instance that receives a message and can take the session's lease
//     delivers it itself. That is the common case, and it costs one claim.
//   - An instance that cannot take the lease writes the message to the shared
//     inbox instead, and the holder drains it on its next poll.
//
// So a session is owned by one instance, moves when that instance dies, and
// never runs in two places at once — which is the failure that would otherwise
// split a conversation in half, with two loops answering the same person from
// two different histories.
package sessionhub

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"agentflow/internal/core/inbox"
	"agentflow/internal/core/lease"
	"agentflow/internal/core/session"
)

// Deliver hands a message to the local session actor. The supervisor supplies
// it; the hub calls it only for sessions this instance owns.
type Deliver func(agent, key string, msg session.Message) error

// Defaults for the two periods. The poll is the added latency for a message
// that arrives on the wrong instance — the price of not needing to know where
// the session is. The session lease is renewed on every poll, so its TTL only
// has to outlive a poll or two: a shorter TTL recovers a dead instance's
// sessions faster, which is the trade a fleet cares about.
const (
	DefaultPoll           = 250 * time.Millisecond
	DefaultSessionTTL     = 30 * time.Second
	DefaultClaimBatchSize = 50
)

// Hub routes messages to sessions and drains what belongs to this instance.
type Hub struct {
	leases  *lease.Manager
	queue   *inbox.Queue
	deliver Deliver
	log     *slog.Logger

	poll time.Duration
	ttl  time.Duration

	mu   sync.Mutex
	held map[string]bool // session keys this instance owns
}

// New builds a hub. leases and queue must live on the same store — they do when
// both follow the runtime persistence target — and deliver is the local
// supervisor.
func New(leases *lease.Manager, queue *inbox.Queue, deliver Deliver, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Hub{
		leases:  leases,
		queue:   queue,
		deliver: deliver,
		log:     log.With("module", "sessionhub"),
		poll:    DefaultPoll,
		ttl:     DefaultSessionTTL,
		held:    map[string]bool{},
	}
}

// SetPoll overrides how often this instance drains the sessions it owns.
func (h *Hub) SetPoll(d time.Duration) {
	if d > 0 {
		h.poll = d
	}
}

// SetSessionTTL overrides how long a session claim survives without renewal.
func (h *Hub) SetSessionTTL(d time.Duration) {
	if d > 0 {
		h.ttl = d
	}
}

// Held reports how many sessions this instance owns.
func (h *Hub) Held() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.held)
}

// Route delivers a message to its session, or leaves it for the owner.
//
// A nil return means the message is handled: delivered here, or durably queued
// for the instance that owns the session. An error means neither — the caller
// may retry, and a retry of the same message id is one row rather than two.
//
// The fast path is the whole point: an instance that already owns the session,
// or can take it because no one else holds it, delivers immediately and the
// extra hop never happens. Only a message that arrives while another instance
// owns the session is queued — and then it is queued durably, so the sending
// instance can die before the owner ever looks at it.
func (h *Hub) Route(ctx context.Context, agent, key string, msg session.Message) error {
	if msg.ID == "" {
		return fmt.Errorf("sessionhub: message for %s|%s has no id", agent, key)
	}
	sessKey := SessionKey(agent, key)
	claimed, err := h.claim(ctx, sessKey)
	if err != nil {
		return err
	}
	if claimed {
		if err := h.deliver(agent, key, msg); err != nil {
			// The delivery failed, but this instance owns the session: queue it
			// rather than fail a message the engine has already accepted, and
			// the drain retries it. That is why a failed delivery still returns
			// nil — the message is not lost, and telling the caller otherwise
			// would invite a retry the queue has already covered.
			h.log.Warn("sessionhub: delivery failed; queueing", "session", sessKey, "id", msg.ID, "err", err)
			return h.queue.Post(ctx, sessKey, msg)
		}
		return nil
	}
	if err := h.queue.Post(ctx, sessKey, msg); err != nil {
		return err
	}
	h.log.Debug("sessionhub: queued for the owning instance", "session", sessKey, "id", msg.ID)
	return nil
}

// claim takes or renews the session's lease, reporting whether this instance
// owns the session now.
func (h *Hub) claim(ctx context.Context, sessKey string) (bool, error) {
	ok, err := h.leases.AcquireFor(ctx, sessionLease+":"+sessKey, h.ttl)
	if err != nil {
		return false, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if ok {
		h.held[sessKey] = true
		return true, nil
	}
	// Another instance owns it, and this one does not.
	delete(h.held, sessKey)
	return false, nil
}

// Drain delivers everything waiting for the sessions this instance owns, and
// runs until ctx is cancelled. It is what makes an instance that received a
// message for someone else's session eventually deliver it, and what lets an
// instance take over a dead peer's sessions along with their backlog.
func (h *Hub) Drain(ctx context.Context) {
	tick := time.NewTicker(h.poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.drainOnce(ctx)
		}
	}
}

// drainOnce is one pass: renew the claims, then deliver what is waiting.
func (h *Hub) drainOnce(ctx context.Context) {
	h.mu.Lock()
	keys := make([]string, 0, len(h.held))
	for k := range h.held {
		keys = append(keys, k)
	}
	h.mu.Unlock()

	for _, sessKey := range keys {
		agent, key, ok := SplitSessionKey(sessKey)
		if !ok {
			continue
		}
		// Renew first: a claim that lapsed while a peer was idle would hand the
		// session over on the next poll, and the renewal is also what tells this
		// instance to stop draining a session it no longer owns.
		if _, err := h.claim(ctx, sessKey); err != nil {
			h.log.Warn("sessionhub: claim renewal failed", "session", sessKey, "err", err)
			continue
		}
		items, err := h.queue.Claim(ctx, h.leases.Owner(), sessKey, DefaultClaimBatchSize)
		if err != nil {
			h.log.Warn("sessionhub: claim failed", "session", sessKey, "err", err)
			continue
		}
		var delivered []inbox.Item
		for _, it := range items {
			if err := h.deliver(agent, key, it.Message); err != nil {
				// Leave it unacked: the next pass tries again, and if this
				// instance dies the visibility window hands it to a peer.
				h.log.Warn("sessionhub: drain delivery failed", "session", sessKey, "id", it.MsgID, "err", err)
				break
			}
			delivered = append(delivered, it)
		}
		if err := h.queue.Ack(ctx, h.leases.Owner(), delivered); err != nil {
			h.log.Warn("sessionhub: ack failed", "session", sessKey, "err", err)
		}
	}
}

// Live reports whether this deployment still owns the session — the question a
// reclaim pass asks before destroying a session's resources. It reads the lease
// rather than this instance's held set, because the session may be alive on
// another instance, and "nobody has it" is the only answer that licenses
// deleting anything.
func (h *Hub) Live(ctx context.Context, sessKey string) (bool, error) {
	return h.leases.Held(ctx, sessionLease+":"+sessKey)
}

// Release gives up every session this instance owns, so a shutdown hands them
// over immediately instead of making the fleet wait out the leases.
func (h *Hub) Release(ctx context.Context) {
	h.mu.Lock()
	keys := make([]string, 0, len(h.held))
	for k := range h.held {
		keys = append(keys, k)
	}
	h.held = map[string]bool{}
	h.mu.Unlock()
	for _, k := range keys {
		if err := h.leases.Release(ctx, sessionLease+":"+k); err != nil {
			h.log.Warn("sessionhub: release failed", "session", k, "err", err)
		}
	}
}

// sessionLease namespaces session claims in the shared lease table.
const sessionLease = "session"

// SessionKey is the identity of a conversation: the same string the supervisor
// keys its actors by, so the hub and the supervisor can never disagree about
// which session a message belongs to.
func SessionKey(agent, key string) string { return agent + "|" + key }

// SplitSessionKey is the inverse of SessionKey.
func SplitSessionKey(s string) (agent, key string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '|' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}
