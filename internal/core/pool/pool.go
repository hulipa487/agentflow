// Package pool is a small fixed-size worker pool with per-tenant fairness.
// Blocking ops (llm.chat, channel sends) run here so the session actor
// goroutine stays responsive to reload and shutdown signals while an op is in
// flight.
//
// Jobs are queued per fairness key — the tenant uuid of the turn that
// submitted them — and workers take the next job round-robin across keys, one
// job at a time per key. One tenant's slow ops therefore hold at most its
// share of the pool instead of every worker, which is the denial-of-service
// vector a single shared FIFO exposed: the queue could not tell whose op it
// was holding. Unkeyed jobs (turns with no tenant) share one queue and join
// the rotation like any other key.
package pool

import (
	"log/slog"
	"sync"
)

// maxQueue is the per-key backlog limit. It was the depth of the old single
// shared buffer; now each tenant gets its own, so a flood blocks its submitter
// at the same depth without touching anyone else's lane.
const maxQueue = 256

// Pool dispatches jobs to a fixed set of workers, fairly across keys.
type Pool struct {
	mu    sync.Mutex
	cond  *sync.Cond // signaled when a job is queued
	space *sync.Cond // signaled when a queued job leaves

	queues map[string][]func()
	order  []string // live keys, arrival order; rotation order
	cursor int      // next slot to inspect in the rotation
	depth  int      // jobs queued across all keys

	log *slog.Logger
}

// Option configures a Pool.
type Option func(*Pool)

// WithLogger sets where job panics are reported. A recovered panic must not
// take the worker down: the pool would shrink silently and every tenant's ops
// would slow with it.
func WithLogger(l *slog.Logger) Option {
	return func(p *Pool) { p.log = l }
}

// New starts n workers.
func New(n int, opts ...Option) *Pool {
	if n < 1 {
		n = 1
	}
	p := &Pool{
		queues: make(map[string][]func()),
		log:    slog.Default(),
	}
	for _, o := range opts {
		o(p)
	}
	p.cond = sync.NewCond(&p.mu)
	p.space = sync.NewCond(&p.mu)
	for i := 0; i < n; i++ {
		go p.worker()
	}
	return p
}

// Submit queues f on the shared (unkeyed) queue.
func (p *Pool) Submit(f func()) { p.SubmitKeyed("", f) }

// SubmitKeyed queues f under key. It blocks while key's backlog is full —
// per-tenant backpressure at the same point the old shared buffer applied it.
func (p *Pool) SubmitKeyed(key string, f func()) {
	p.mu.Lock()
	for len(p.queues[key]) >= maxQueue {
		p.space.Wait()
	}
	if _, ok := p.queues[key]; !ok {
		p.order = append(p.order, key)
	}
	p.queues[key] = append(p.queues[key], f)
	p.depth++
	p.mu.Unlock()
	p.cond.Signal()
}

// Call runs f on a worker and returns a channel that receives f's result.
// Callers typically select on the result channel against reload/shutdown
// signals; an abandoned result is discarded (buffered, never blocks).
func Call[T any](p *Pool, f func() T) <-chan T {
	return CallKeyed(p, "", f)
}

// CallKeyed is Call with a fairness key.
func CallKeyed[T any](p *Pool, key string, f func() T) <-chan T {
	ch := make(chan T, 1)
	p.SubmitKeyed(key, func() { ch <- f() })
	return ch
}

// Queued reports the total backlog across all keys.
func (p *Pool) Queued() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.depth
}

func (p *Pool) worker() {
	for {
		p.mu.Lock()
		for p.depth == 0 {
			p.cond.Wait()
		}
		f := p.popNext()
		p.space.Broadcast()
		p.mu.Unlock()
		p.run(f)
	}
}

// popNext takes the head of the next non-empty queue in the rotation. The
// cursor marks where the last dispatch left off, so each key gets one job per
// full turn of the ring regardless of how deep its queue is. Caller holds mu;
// depth > 0.
func (p *Pool) popNext() func() {
	key := p.order[p.cursor]
	q := p.queues[key]
	f := q[0]
	p.depth--
	if len(q) == 1 {
		delete(p.queues, key)
		p.order = append(p.order[:p.cursor], p.order[p.cursor+1:]...)
		// The key that shifted into this cursor slot is the next one in the
		// old rotation; wrap only if the removal emptied the ring.
		if p.cursor == len(p.order) {
			p.cursor = 0
		}
		return f
	}
	p.queues[key] = q[1:]
	p.cursor = (p.cursor + 1) % len(p.order)
	return f
}

func (p *Pool) run(f func()) {
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("pool: job panicked; worker survives", "panic", r)
		}
	}()
	f()
}
