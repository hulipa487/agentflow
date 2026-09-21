// Package runtime owns the engine's operational state: the message journal,
// the key/value rows the file store keeps its metadata in, and the per-user
// token ledger.
//
// Two implementations exist behind the Store interface. The SQLite one is the
// single-instance default — a local file, WAL, no server to run. The PostgreSQL
// one is what a fleet needs: several instances sharing one logical store, so a
// journal row, a commit or a token count means the same thing to all of them.
// Nothing above this package knows which is in use.
package runtime

import (
	"context"
	"io"
	"log/slog"
	"time"

	"agentflow/internal/core/storedb"
)

// Store is the runtime store's surface. Both implementations satisfy it, and
// every consumer above takes this interface rather than a concrete type.
type Store interface {
	// --- message journal (the core-owned audit trail) ---
	RecordMessage(ctx context.Context, e JournalEntry) error
	ListMessages(ctx context.Context, f JournalFilter) ([]JournalEntry, error)
	PruneMessages(ctx context.Context, cutoff time.Time) (int64, error)
	JournalRowCount(ctx context.Context) (int64, error)

	// --- key/value rows (file-store metadata: trees, commits, refs, scratch) ---
	PutRow(ctx context.Context, key, value string, expiresAt time.Time) error
	GetRow(ctx context.Context, key string) (Row, bool, error)
	DeleteRow(ctx context.Context, key string) error
	ListRows(ctx context.Context, prefix string) ([]Row, error)
	SweepExpired(ctx context.Context) (int, error)

	// --- per-user token ledger ---
	RecordUsage(rec UsageRecord, withEvent bool) error
	UsageForDay(userID, day string) (UsageTotals, error)
	UsageHistory(userID string, days int) ([]UsageRow, error)
	UsageDaily(day string) ([]UsageRow, error)
	UsageEvents(userID string, since time.Time, limit int) ([]UsageEvent, error)
	PruneUsage(before time.Time) error

	// Path identifies the store for logs. A PostgreSQL store reports a
	// credential-free form of its DSN: a store's address may be logged, its
	// password must not be.
	Path() string
	Close() error
}

// Backend names the storage engine behind the runtime store.
const (
	BackendSQLite   = storedb.BackendSQLite
	BackendPostgres = storedb.BackendPostgres
)

// BackendFor reports which implementation a persistence target selects. The
// scheme decides: a postgres:// or postgresql:// target is a server, anything
// else is a SQLite file path (the config layer strips any "sqlite://" prefix
// before it reaches here).
func BackendFor(target string) string { return storedb.BackendFor(target) }

// OpenStore opens the runtime store a configuration selects: a PostgreSQL
// server when the persistence target is a postgres DSN, else a local SQLite
// file. It is the only constructor main needs.
func OpenStore(target string, log *slog.Logger) (Store, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	switch BackendFor(target) {
	case BackendPostgres:
		s, err := OpenPostgres(target)
		if err != nil {
			return nil, err
		}
		log.Info("runtime store ready", "backend", BackendPostgres, "target", s.Path())
		return s, nil
	default:
		s, err := OpenSQLite(target)
		if err != nil {
			return nil, err
		}
		log.Info("runtime store ready", "backend", BackendSQLite, "target", s.Path())
		return s, nil
	}
}

// redactDSN strips credentials from a PostgreSQL DSN so it can be logged.
func redactDSN(dsn string) string { return storedb.RedactDSN(dsn) }
