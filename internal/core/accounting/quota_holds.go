package accounting

import (
	"context"
	"hash/fnv"
	"time"

	"github.com/google/uuid"

	"agentflow/internal/core/runtime"
	"agentflow/internal/core/storedb"
)

// holdTTL is how long an in-flight reservation survives without being released.
// It must outlive any model call (a long generation can take minutes), because
// a hold that expires mid-call stops bounding the call it was taken for. Ten
// minutes is deliberately generous; the row is normally deleted the moment the
// call ends.
const holdTTL = 10 * time.Minute

// quotaHoldsSchema is the shared reservation table. Deadlines are unix
// nanoseconds, the same convention the lease and inbox tables use.
var quotaHoldsSchema = []string{
	`CREATE TABLE IF NOT EXISTS quota_holds (
		id         TEXT PRIMARY KEY,
		user_id    TEXT NOT NULL,
		amount     BIGINT NOT NULL,
		expires_at BIGINT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS quota_holds_user ON quota_holds (user_id, expires_at)`,
}

// NewShared builds a regional quota: reservations live in the runtime store, so
// every instance sees the same in-flight total. The caller must Close it.
func NewShared(target string, ledger runtime.Ledger, limitFn func(userID string) (int64, error), defaultPerDay int64) (*Quota, error) {
	st, err := storedb.Open(target)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.ExecDDL(ctx, quotaHoldsSchema...); err != nil {
		_ = st.Close()
		return nil, err
	}
	q := &Quota{
		ledger:  ledger,
		limit:   limitFn,
		Default: defaultPerDay,
		now:     time.Now,
	}
	q.holds = &sqlHolds{st: st, ledger: ledger, now: func() time.Time { return q.now() }}
	return q, nil
}

// Close releases the shared store. It is a no-op for the in-memory quota.
func (q *Quota) Close() error {
	if c, ok := q.holds.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// SweepHolds deletes reservations that expired before the cutoff. Expired holds
// are already ignored by the checks; this keeps the table from growing. It is a
// no-op for the in-memory quota.
func (q *Quota) SweepHolds(ctx context.Context, before time.Time) (int64, error) {
	if s, ok := q.holds.(*sqlHolds); ok {
		return s.sweep(ctx, before)
	}
	return 0, nil
}

// sqlHolds is the regional reservation store: rows in the runtime store.
type sqlHolds struct {
	st     *storedb.DB
	ledger runtime.Ledger
	now    func() time.Time
}

// Reserve takes a hold atomically. The per-user advisory lock serialises the
// check-and-insert across instances: without it two transactions could both
// read the same in-flight total and each admit a call. It is taken before the
// ledger read, because the ledger read has to happen after the previous
// holder's usage write is visible — which it is, since that holder records
// usage before releasing its hold.
func (s *sqlHolds) Reserve(ctx context.Context, userID string, amount, limit int64) (string, int64, int64, bool, error) {
	tx, err := s.st.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, 0, false, err
	}
	defer func() { _ = tx.Rollback() }()

	if s.st.Backend() == storedb.BackendPostgres {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(?)`, advisoryKey(userID)); err != nil {
			return "", 0, 0, false, err
		}
	}

	spent, err := s.ledger.UsageForDay(userID, runtime.DayKey(s.now()))
	if err != nil {
		return "", 0, 0, false, err
	}
	now := s.now().UnixNano()
	var held int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0) FROM quota_holds WHERE user_id = ? AND expires_at > ?`,
		userID, now).Scan(&held); err != nil {
		return "", 0, 0, false, err
	}
	used := spent.Billable()
	if used+held+amount > limit {
		return "", used, held, false, nil
	}
	id := uuid.NewString()
	if _, err := tx.Exec(ctx,
		`INSERT INTO quota_holds (id, user_id, amount, expires_at) VALUES (?, ?, ?, ?)`,
		id, userID, amount, now+int64(holdTTL)); err != nil {
		return "", 0, 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, 0, false, err
	}
	return id, used, held, true, nil
}

func (s *sqlHolds) Release(ctx context.Context, id, _ string, _ int64) error {
	if id == "" {
		return nil
	}
	_, err := s.st.Exec(ctx, `DELETE FROM quota_holds WHERE id = ?`, id)
	return err
}

func (s *sqlHolds) InFlight(ctx context.Context, userID string) (int64, error) {
	var held int64
	err := s.st.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0) FROM quota_holds WHERE user_id = ? AND expires_at > ?`,
		userID, s.now().UnixNano()).Scan(&held)
	return held, err
}

func (s *sqlHolds) sweep(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.st.Exec(ctx, `DELETE FROM quota_holds WHERE expires_at < ?`, before.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *sqlHolds) Close() error { return s.st.Close() }

// advisoryKey maps a user id onto PostgreSQL's bigint advisory-lock space. A
// collision only serialises two unrelated users' reservations for the length of
// one transaction, which is harmless.
func advisoryKey(userID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(userID))
	return int64(h.Sum64())
}
