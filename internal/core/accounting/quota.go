// Package accounting enforces per-user token quotas against the durable ledger.
//
// A quota is deliberately not an in-memory pool. The agent budget pool resets
// on restart because it bounds one agent's instantaneous appetite; a per-user
// quota is an account, so it reads the ledger and a restart forgives nothing.
// What is held is only the in-flight reservation, which exists to stop two
// concurrent calls from each spending the last of a user's budget.
//
// Where that reservation lives depends on the tier. A single instance keeps it
// in memory, as it always did. A regional cell keeps it in the shared store:
// two instances admitting a call each against the same remaining budget is
// exactly the overspend the reservation exists to prevent, and process-local
// maps cannot see each other.
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

// holds is the in-flight reservation store behind a quota. Reserve must be
// atomic with respect to other reservations for the same user: the whole point
// is that two concurrent calls cannot both be admitted against the last of a
// budget. Implementations return the used and in-flight figures they checked,
// so the refusal message names the real numbers.
type holds interface {
	Reserve(ctx context.Context, userID string, amount, limit int64) (id string, used, inFlight int64, ok bool, err error)
	Release(ctx context.Context, id, userID string, amount int64) error
	InFlight(ctx context.Context, userID string) (int64, error)
}

// Quota bounds how many billable tokens a user may spend per UTC day.
type Quota struct {
	ledger runtime.Ledger
	// limit resolves a profile's own daily limit (0 = no override). It is a
	// function rather than a profile-store dependency so this package stays
	// clear of the identity and router packages, which the caps package — its
	// caller — already reaches transitively.
	limit func(userID string) (int64, error)
	// Default is the deployment-wide daily limit; 0 means unlimited.
	Default int64
	now     func() time.Time
	holds   holds
}

// New builds a single-instance quota: reservations live in this process.
func New(ledger runtime.Ledger, limitFn func(userID string) (int64, error), defaultPerDay int64) *Quota {
	q := &Quota{
		ledger:  ledger,
		limit:   limitFn,
		Default: defaultPerDay,
		now:     time.Now,
	}
	q.holds = &memoryHolds{
		ledger:   ledger,
		now:      func() time.Time { return q.now() },
		reserved: map[string]int64{},
	}
	return q
}

// Lease is one in-flight reservation.
type Lease struct {
	q        *Quota
	id       string
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
	id, used, inFlight, ok, err := q.holds.Reserve(ctx, userID, amount, limit)
	if err != nil {
		return nil, fmt.Errorf("quota read: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: %d of %d tokens used today (%d in flight)",
			ErrQuotaExhausted, used, limit, inFlight)
	}
	return &Lease{q: q, id: id, userID: userID, amount: amount}, nil
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
	// Releasing is best-effort by design: a hold that cannot be deleted in the
	// shared store expires on its own, which errs toward refusing a call rather
	// than admitting one that should not fit.
	_ = l.q.holds.Release(context.Background(), l.id, l.userID, l.amount)
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
	inFlight, err = q.holds.InFlight(context.Background(), userID)
	if err != nil {
		return 0, 0, 0, err
	}
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

// memoryHolds is the single-instance reservation store: a map in this process.
type memoryHolds struct {
	ledger runtime.Ledger
	now    func() time.Time

	mu       sync.Mutex
	reserved map[string]int64 // user → in-flight estimate
}

func (m *memoryHolds) Reserve(_ context.Context, userID string, amount, limit int64) (string, int64, int64, bool, error) {
	spent, err := m.ledger.UsageForDay(userID, runtime.DayKey(m.now()))
	if err != nil {
		return "", 0, 0, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	held := m.reserved[userID]
	used := spent.Billable()
	if used+held+amount > limit {
		return "", used, held, false, nil
	}
	m.reserved[userID] = held + amount
	return "", used, held, true, nil
}

func (m *memoryHolds) Release(_ context.Context, _, userID string, amount int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	left := m.reserved[userID] - amount
	if left > 0 {
		m.reserved[userID] = left
	} else {
		delete(m.reserved, userID)
	}
	return nil
}

func (m *memoryHolds) InFlight(_ context.Context, userID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reserved[userID], nil
}
