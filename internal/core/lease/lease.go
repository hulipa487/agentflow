// Package lease is a distributed mutex over the engine's shared store: it
// decides which instance owns a piece of singleton work at a given moment.
//
// The engine's scheduled work — every:/cron: triggers, the file GC, the
// retention prunes — has to run once per *deployment*, not once per instance.
// In a single-instance deployment a lease is uncontended and costs one row
// update; in a fleet it is what keeps two instances from firing the same
// trigger, or sweeping the same blob store while the other is writing to it.
//
// It is a row in the shared store rather than a lock service, for the same
// reason the rest of the engine's state is: the store is already there, and a
// deployment that has one has the lease. Holding it is a compare-and-set on
// (owner, expiry) — the classic design, with the classic trade: the deadline
// comes from the machines' clocks rather than a trusted one, and a holder that
// dies is replaced when its lease expires rather than immediately. Both are
// acceptable for work that is periodic by nature.
package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"agentflow/internal/core/storedb"
)

// DefaultTTL is how long a lease survives without being renewed. It has to
// exceed the gap between renewals — a trigger renews on every fire, a sweep
// renews by finishing — so it is set well beyond any normal interval: a lease
// that expires between two fires of the same instance is not a bug, but a
// takeover is only useful when the holder is genuinely gone.
const DefaultTTL = 2 * time.Minute

// schema is the lease table, written once for both backends. Deadlines are unix
// nanoseconds, the same convention the file store's expiry rows use: a lease
// TTL should mean what it says, and second granularity would let a one-second
// lease live for nearly two.
var schema = `CREATE TABLE IF NOT EXISTS leases (
	name       TEXT PRIMARY KEY,
	owner      TEXT NOT NULL,
	expires_at BIGINT NOT NULL
)`

// Manager hands out and renews leases for one process. Its owner string
// identifies this instance: two processes must never share one, or each would
// renew the other's leases.
type Manager struct {
	st    *storedb.DB
	owner string
	ttl   time.Duration
	log   *slog.Logger
}

// Open creates a lease manager over the store at target — the same target the
// runtime store uses, which in a fleet is the shared server. owner must be
// unique to this process (OwnerID builds one); ttl <= 0 takes DefaultTTL.
func Open(target, owner string, ttl time.Duration, log *slog.Logger) (*Manager, error) {
	if owner == "" {
		return nil, fmt.Errorf("lease: empty owner")
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	st, err := storedb.Open(target)
	if err != nil {
		return nil, fmt.Errorf("lease: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.ExecDDL(ctx, schema); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("lease: %w", err)
	}
	return &Manager{st: st, owner: owner, ttl: ttl, log: log.With("module", "lease")}, nil
}

// Owner is this instance's identifier, for logs.
func (m *Manager) Owner() string { return m.owner }

// Close releases this manager's hold on the connection pool. Leases it holds
// are left to expire: a lease outliving its holder is the point of the expiry.
func (m *Manager) Close() error { return m.st.Close() }

// Acquire takes the named lease for DefaultTTL, or renews it if this instance
// already holds it, and reports whether this instance holds it now.
//
// A false is not an error: it means another instance is the owner, and the
// caller's work is that instance's to do. The statement is a single
// compare-and-set, so two instances calling it at the same instant cannot both
// be told yes — which is what makes "fire this occurrence" happen exactly once,
// wherever the timers happen to be.
func (m *Manager) Acquire(ctx context.Context, name string) (bool, error) {
	return m.AcquireFor(ctx, name, m.ttl)
}

// AcquireFor is Acquire with an explicit deadline instead of the manager's
// default. A caller whose work repeats should claim for as long as it means to
// own the work: a claim that lapses between two occurrences leaves the next one
// to whoever asks first, which for a phase-offset timer means the schedule
// quietly runs twice.
func (m *Manager) AcquireFor(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = m.ttl
	}
	now := time.Now().UnixNano()
	expires := now + int64(ttl)
	res, err := m.st.Exec(ctx, `
		INSERT INTO leases (name, owner, expires_at) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			owner = excluded.owner,
			expires_at = excluded.expires_at
		WHERE leases.expires_at < ? OR leases.owner = ?`,
		name, m.owner, expires, now, m.owner)
	if err != nil {
		return false, fmt.Errorf("lease: acquire %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("lease: acquire %q: %w", name, err)
	}
	if n == 0 {
		return false, nil
	}
	m.log.Debug("lease acquired", "name", name, "ttl", ttl.String())
	return true, nil
}

// Release gives up a lease this instance holds, so another instance can take it
// immediately rather than waiting for the expiry. Releasing a lease this
// instance does not hold does nothing: a stale instance cannot evict the live
// owner.
func (m *Manager) Release(ctx context.Context, name string) error {
	_, err := m.st.Exec(ctx,
		`DELETE FROM leases WHERE name = ? AND owner = ?`, name, m.owner)
	if err != nil {
		return fmt.Errorf("lease: release %q: %w", name, err)
	}
	return nil
}

// OwnerID builds an owner string for this process: the host, the pid, and a
// random suffix. The host and pid make a log line readable; the random part
// makes it unique, which matters where hostnames repeat — containers, or a
// process that restarted into the same pid. Two processes renewing each other's
// leases would defeat the point.
func OwnerID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// A clock-based fallback would be worse than useless here: a collision
		// makes two instances believe they are one.
		panic("lease: crypto/rand: " + err.Error())
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b))
}
