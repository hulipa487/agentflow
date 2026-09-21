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
	"io"
	"log/slog"

	"agentflow/internal/core/storedb"
)

// Store and its four narrow planes (Journal, Rows, Ledger, EventLog) are
// declared in interfaces.go. Both implementations satisfy the whole of Store;
// a consumer takes the part it needs.

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
