// Package budget meters LLM token usage with parent/child pool allocation.
// Reservation happens before a provider call; the reported usage is committed
// after the call returns. Unused reservation is released.
package budget

import (
	"fmt"
	"sync"
	"time"
)

// Pool is a token budget. A parent pool may have child pools carved from it.
type Pool struct {
	mu      sync.Mutex
	limit   int64
	used    int64
	reserved int64
	parent  *Pool
}

// NewPool creates a root pool with the given daily token limit.
func NewPool(limit int64) *Pool {
	return &Pool{limit: limit}
}

// Carve creates a child pool with its own limit, drawing from the parent's
// remaining capacity. The child's limit cannot exceed the parent's remaining
// unreserved budget.
func (p *Pool) Carve(limit int64) (*Pool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	available := p.limit - p.used - p.reserved
	if limit > available {
		return nil, fmt.Errorf("budget: cannot carve %d from parent with %d available", limit, available)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("budget: child limit must be positive")
	}
	p.reserved += limit
	return &Pool{limit: limit, parent: p}, nil
}

// Lease represents a reservation that must be settled.
type Lease struct {
	pool     *Pool
	amount   int64
	released bool
}

// Reserve attempts to reserve tokens before an LLM call. Returns an error if
// the pool (or any ancestor) cannot accommodate the reservation.
func (p *Pool) Reserve(amount int64) (*Lease, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("budget: reserve amount must be positive")
	}
	if !p.tryReserve(amount) {
		return nil, ErrExhausted
	}
	return &Lease{pool: p, amount: amount}, nil
}

func (p *Pool) tryReserve(amount int64) bool {
	// Walk from the leaf to the root, checking capacity at every level.
	// This is a simple two-level hierarchy; deeper nesting is not supported.
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used+p.reserved+amount > p.limit {
		return false
	}
	// Also check the parent has room for this child's reservation.
	if p.parent != nil {
		if !p.parent.reserveFromChild(amount) {
			return false
		}
	}
	p.reserved += amount
	return true
}

func (p *Pool) reserveFromChild(amount int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used+p.reserved+amount > p.limit {
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
	p.reserved -= lease.amount
	if actual < 0 {
		actual = 0
	}
	p.used += actual
	p.mu.Unlock()
	if p.parent != nil {
		p.parent.releaseFromChild(lease.amount)
		p.parent.commitFromChild(actual)
	}
	return nil
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
	if p.parent != nil {
		p.parent.releaseFromChild(lease.amount)
	}
}

func (p *Pool) releaseFromChild(amount int64) {
	p.mu.Lock()
	p.reserved -= amount
	p.mu.Unlock()
}

func (p *Pool) commitFromChild(actual int64) {
	p.mu.Lock()
	p.used += actual
	p.mu.Unlock()
}

// Remaining returns the tokens still available (limit - used - reserved).
func (p *Pool) Remaining() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.limit - p.used - p.reserved
}

// Used returns the committed usage.
func (p *Pool) Used() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.used
}

// ResetDaily resets the used counter at day boundaries (UTC).
func (p *Pool) ResetDaily() {
	p.mu.Lock()
	p.used = 0
	p.mu.Unlock()
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
