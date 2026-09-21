// Package accounting enforces per-user token quotas against the durable ledger.
//
// A quota is deliberately not an in-memory pool. The agent budget pool resets
// on restart because it bounds one agent's instantaneous appetite; a per-user
// quota is an account, so it reads the ledger and a restart forgives nothing.
// What is held in memory is only the in-flight reservation, which exists to
// stop two concurrent calls from each spending the last of a user's budget.
package accounting

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"agentflow/internal/core/runtime"
)

// ErrQuotaExhausted reports that a user has no room left in the current window.
var ErrQuotaExhausted = errors.New("user quota exhausted")

// Quota bounds how many billable tokens a user may spend per UTC day.
type Quota struct {
	ledger *runtime.Store
	// limit resolves a profile's own daily limit (0 = no override). It is a
	// function rather than a profile-store dependency so this package stays
	// clear of the identity and router packages, which the caps package — its
	// caller — already reaches transitively.
	limit func(userID string) (int64, error)
	// Default is the deployment-wide daily limit; 0 means unlimited.
	Default int64
	now     func() time.Time

	mu       sync.Mutex
	reserved map[string]int64 // user → in-flight estimate
}

// New builds a quota check over the ledger. limitFn may be nil, in which case
// every user falls back to defaultPerDay.
func New(ledger *runtime.Store, limitFn func(userID string) (int64, error), defaultPerDay int64) *Quota {
	return &Quota{
		ledger:   ledger,
		limit:    limitFn,
		Default:  defaultPerDay,
		now:      time.Now,
		reserved: map[string]int64{},
	}
}

// Lease is one in-flight reservation.
type Lease struct {
	q        *Quota
	userID   string
	amount   int64
	released bool
}

// Reserve checks the user's remaining daily budget and holds amount against it.
// An empty userID (engine-fired work with no user) has no quota, and a limit of
// zero means unlimited: both return an empty lease and no error.
func (q *Quota) Reserve(ctx context.Context, userID string, amount int64) (*Lease, error) {
	if q == nil || userID == "" {
		return nil, nil
	}
	limit, err := q.limitFor(userID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	spent, err := q.ledger.UsageForDay(userID, runtime.DayKey(q.now()))
	if err != nil {
		return nil, fmt.Errorf("quota read: %w", err)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	held := q.reserved[userID]
	used := spent.Billable()
	if used+held+amount > limit {
		return nil, fmt.Errorf("%w: %d of %d tokens used today (%d in flight)",
			ErrQuotaExhausted, used, limit, held)
	}
	q.reserved[userID] = held + amount
	return &Lease{q: q, userID: userID, amount: amount}, nil
}

// Commit ends a reservation. The durable record is the ledger row the caller
// writes with RecordUsage — this only releases the in-flight hold, so callers
// must record before committing or a concurrent call can over-admit.
func (l *Lease) Commit(actual int64) { l.end() }

// Release ends a reservation without a spend (a denied or failed call).
func (l *Lease) Release() { l.end() }

func (l *Lease) end() {
	if l == nil || l.q == nil || l.released {
		return
	}
	l.released = true
	l.q.mu.Lock()
	defer l.q.mu.Unlock()
	left := l.q.reserved[l.userID] - l.amount
	if left > 0 {
		l.q.reserved[l.userID] = left
		return
	}
	delete(l.q.reserved, l.userID)
}

// Status reports a user's usage today: billable tokens spent, the limit in
// force (0 = unlimited), and the tokens still in flight.
func (q *Quota) Status(userID string) (used, limit, inFlight int64, err error) {
	if q == nil {
		return 0, 0, 0, nil
	}
	limit, err = q.limitFor(userID)
	if err != nil {
		return 0, 0, 0, err
	}
	spent, err := q.ledger.UsageForDay(userID, runtime.DayKey(time.Now()))
	if err != nil {
		return 0, 0, 0, err
	}
	q.mu.Lock()
	inFlight = q.reserved[userID]
	q.mu.Unlock()
	return spent.Billable(), limit, inFlight, nil
}

// limitFor resolves the limit in force for a user: their profile override when
// set, else the deployment default. It reads through on every call — a profile
// edit takes effect at the next call rather than at the next restart, and a
// keyed read against a local database is far cheaper than the LLM call it
// gates.
func (q *Quota) limitFor(userID string) (int64, error) {
	if q.limit != nil {
		own, err := q.limit(userID)
		if err != nil {
			return 0, err
		}
		if own > 0 {
			return own, nil
		}
	}
	return q.Default, nil
}
