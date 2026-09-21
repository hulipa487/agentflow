// Package budget meters LLM token usage with pre-call reservation and
// post-call commit. Each agent gets a root pool; unused reservation is
// released after the provider call returns.
package budget

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Pool is a token budget.
//
// Accounting has two modes:
//   - Daily reset (default): `used` accumulates until ResetDaily zeroes it at
//     the UTC day boundary. This is the historical tokens_per_day behavior.
//   - Rolling window: when window > 0, every commit is appended to a timestamped
//     log and the effective usage is the sum over the trailing window. Reserve
//     and capacity checks read the windowed sum, so budget frees up continuously
//     as old commits age out instead of only at midnight.
type Pool struct {
	mu       sync.Mutex
	limit    int64
	used     int64
	reserved int64
	window   time.Duration // 0 = daily-reset mode
	commits  []commit      // windowed-mode commit log, oldest first
	now      func() time.Time

	// source, when set, is where the *deployment-wide* usage for the current
	// day comes from: the token ledger. base is what it reported when last
	// read, and used counts what this process has committed since. Without a
	// source a pool counts only its own process — which in a fleet hands every
	// instance the full budget, and forgives everything spent when one restarts.
	source    func(context.Context) (int64, error)
	base      int64
	baseAt    time.Time
	refreshIn time.Duration
}

// DefaultRefreshInterval is how stale a ledger reading may be before the next
// reservation refreshes it. Every instance converges on the deployment's real
// spend within this interval, so the overshoot a fleet can reach is bounded by
// what its instances spend in one interval rather than by the instance count.
const DefaultRefreshInterval = 15 * time.Second

type commit struct {
	ts     time.Time
	amount int64
}

// NewPool creates a root pool with the given daily token limit.
func NewPool(limit int64) *Pool {
	return &Pool{limit: limit, now: time.Now}
}

// SetWindow enables rolling-window accounting over the given duration (e.g.
// 168h for a weekly budget). The daily reset is a no-op in windowed mode;
// usage drains continuously as commits age past the window.
func (p *Pool) SetWindow(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.window = d
}

// windowedUsed prunes expired commits and returns the trailing-window sum.
// Caller must hold p.mu.
func (p *Pool) windowedUsed() int64 {
	if p.window <= 0 {
		return p.used
	}
	cutoff := p.now().Add(-p.window)
	kept := p.commits[:0]
	var sum int64
	for _, c := range p.commits {
		if c.ts.After(cutoff) {
			kept = append(kept, c)
			sum += c.amount
		}
	}
	p.commits = kept
	p.used = sum
	return sum
}

// Lease represents a reservation that must be settled.
type Lease struct {
	pool     *Pool
	amount   int64
	released bool
}

// SetUsageSource installs where this pool's deployment-wide usage comes from:
// the token ledger, summed over every instance and every user the agent served.
// It is what makes an agent's budget an agent's budget rather than a
// per-process one, and what stops a restart from forgiving the day's spend.
//
// The source is consulted by Refresh, at most once per refresh interval. It
// applies to daily accounting only: a rolling window is this process's own
// commits over the trailing period, which the ledger's day-granular rows cannot
// express.
func (p *Pool) SetUsageSource(src func(context.Context) (int64, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.source = src
	p.baseAt = time.Time{} // read it on the next reservation
}

// SetRefreshInterval overrides how often the source is consulted.
func (p *Pool) SetRefreshInterval(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.refreshIn = d
	}
}

// Refresh re-reads the shared usage figure when the cached one is stale. The
// local count restarts from zero because what this process committed since the
// last read is in the ledger by then — the ledger is the whole truth, and the
// alternative double-counts this instance's own calls.
//
// It is best-effort: an unreadable ledger leaves the pool counting what it
// knows, which is what a pool with no source does anyway. Accounting failing
// must not stop the runtime.
func (p *Pool) Refresh(ctx context.Context) { p.refreshIfStale(ctx) }

func (p *Pool) refreshIfStale(ctx context.Context) {
	p.mu.Lock()
	src := p.source
	interval := p.refreshIn
	if interval <= 0 {
		interval = DefaultRefreshInterval
	}
	stale := src != nil && p.window <= 0 &&
		(p.baseAt.IsZero() || p.now().Sub(p.baseAt) >= interval)
	p.mu.Unlock()
	if !stale {
		return
	}

	total, err := src(ctx)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.base = total
	p.used = 0
	p.baseAt = p.now()
}

// usedLocked is the figure every check and every reading uses: the
// deployment's total as of the last refresh, plus what this process has
// committed since.
func (p *Pool) usedLocked() int64 {
	if p.window > 0 {
		return p.windowedUsed()
	}
	return p.base + p.used
}

// Reserve attempts to reserve tokens before an LLM call. Returns an error if
// the pool cannot accommodate the reservation.
//
// A pool with a ledger source refreshes here if it is stale, with a background
// context: no caller can forget, and a call is never refused — or allowed — on
// a figure that is only this process's. Callers on a request path pass their
// own context to Refresh first, which is the same read with the caller's
// deadline and cancellation.
func (p *Pool) Reserve(amount int64) (*Lease, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("budget: reserve amount must be positive")
	}
	p.refreshIfStale(context.Background())
	if !p.tryReserve(amount) {
		return nil, ErrExhausted
	}
	return &Lease{pool: p, amount: amount}, nil
}

func (p *Pool) tryReserve(amount int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.usedLocked()+p.reserved+amount > p.limit {
		return false
	}
	p.reserved += amount
	return true
}

// Commit settles a lease with the actual usage. Unused reservation is released.
func (p *Pool) Commit(lease *Lease, actual int64) error {
	if lease == nil || lease.released {
		return fmt.Errorf("budget: lease already settled or nil")
	}
	lease.released = true
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reserved -= lease.amount
	if actual < 0 {
		actual = 0
	}
	p.recordLocked(actual)
	return nil
}

// recordLocked accounts actual usage. Caller must hold p.mu.
func (p *Pool) recordLocked(actual int64) {
	if p.window > 0 {
		p.commits = append(p.commits, commit{ts: p.now(), amount: actual})
	}
	p.used += actual
}

// Release cancels a lease without consuming any budget.
func (p *Pool) Release(lease *Lease) {
	if lease == nil || lease.released {
		return
	}
	lease.released = true
	p.mu.Lock()
	p.reserved -= lease.amount
	p.mu.Unlock()
}

// Remaining returns the tokens still available (limit - used - reserved).
func (p *Pool) Remaining() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.limit - p.usedLocked() - p.reserved
}

// Used returns the committed usage: this deployment's, when a ledger source is
// installed, else this process's (the trailing-window sum in windowed mode).
func (p *Pool) Used() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.usedLocked()
}

// ResetDaily resets the used counter at day boundaries (UTC). In windowed mode
// this is a no-op: usage drains continuously as commits age past the window, so
// there is no daily cliff to reset.
func (p *Pool) ResetDaily() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.window > 0 {
		return
	}
	p.used = 0
	// The day rolled over, so the ledger's figure for "today" is zero too. A
	// stale baseline would keep the pool exhausted into the new day.
	p.base = 0
	p.baseAt = time.Time{}
}

// StartDailyReset starts a goroutine that resets usage at the next UTC midnight
// and every 24 hours thereafter. Returns a cancel function that is safe to call
// multiple times.
func (p *Pool) StartDailyReset() (stop func()) {
	stopCh := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			now := time.Now().UTC()
			next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
			timer := time.NewTimer(time.Until(next))
			select {
			case <-timer.C:
				p.ResetDaily()
			case <-stopCh:
				timer.Stop()
				return
			}
		}
	}()
	return func() { once.Do(func() { close(stopCh) }) }
}

// ErrExhausted is returned when a pool cannot accommodate a reservation.
var ErrExhausted = fmt.Errorf("budget exhausted")
