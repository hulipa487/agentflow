// Package storedb opens the engine's SQL stores on either backend.
//
// Every store the engine owns — the runtime journal, the identity registry, the
// credential store — speaks one dialect: statements written with "?"
// placeholders and TEXT/BIGINT columns, which SQLite and PostgreSQL both
// accept. What genuinely differs between the backends is small, and it lives
// here: how a connection is opened (a file with pragmas, or a bounded pool
// against a server), how parameters are bound, and how a target is named in a
// log line.
//
// The point is that a store's SQL is written once and runs on both. A second
// hand-written implementation per backend would be a second chance to get a
// security-relevant statement subtly wrong — and the two would drift.
package storedb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Backend names the storage engine a target selects.
const (
	BackendSQLite   = "sqlite"
	BackendPostgres = "postgres"
)

// BackendFor reports which backend a target selects. The scheme decides: a
// postgres:// or postgresql:// target is a server, anything else is a SQLite
// file path (the config layer strips any "sqlite://" prefix before it reaches
// here).
func BackendFor(target string) string {
	switch {
	case strings.HasPrefix(target, "postgres://"), strings.HasPrefix(target, "postgresql://"):
		return BackendPostgres
	default:
		return BackendSQLite
	}
}

// maxOpenPostgres bounds a server pool. This is one number for the whole
// process: every store pointed at the same server shares one pool (see Open),
// so it is the instance's budget against the database rather than a per-store
// one — a fleet multiplies it by its instance count, and an unbounded pool is
// how a fleet takes its own database down. A PostgreSQL server's default
// connection limit is 100.
const maxOpenPostgres = 10

// pool is one *sql.DB and the number of stores holding it.
type pool struct {
	db   *sql.DB
	refs int
}

var (
	poolMu sync.Mutex
	pools  = map[string]*pool{}
)

// DB is a SQL store on either backend. Its statements are written with "?"
// placeholders and bound the way the driver expects; nothing above this
// package knows which backend it is talking to.
type DB struct {
	db      *sql.DB
	backend string
	target  string // a file path, or a credential-free DSN, for logs
	key     string // the pool key, held for the refcount on Close
}

// Open opens a store: a SQLite file when the target is a path, a PostgreSQL
// server when it is a DSN. Stores opened on the same target in one process
// share a single connection pool — in a fleet every store points at the same
// server, and three pools per instance is three times the connections for no
// benefit.
//
// Migrating is the caller's job: schema is per store, and a shared pool must
// not decide what a store's tables look like.
func Open(target string) (*DB, error) {
	backend := BackendFor(target)
	key := target
	if backend == BackendSQLite {
		if target == "" {
			return nil, fmt.Errorf("storedb: empty sqlite path")
		}
		abs, err := filepath.Abs(target)
		if err != nil {
			abs = target
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0750); err != nil {
			return nil, fmt.Errorf("storedb: create directory for %s: %w", abs, err)
		}
		key = abs
	}

	poolMu.Lock()
	defer poolMu.Unlock()
	if p, ok := pools[key]; ok {
		p.refs++
		return &DB{db: p.db, backend: backend, target: displayTarget(backend, key), key: key}, nil
	}

	db, err := dial(backend, key)
	if err != nil {
		return nil, err
	}
	pools[key] = &pool{db: db, refs: 1}
	return &DB{db: db, backend: backend, target: displayTarget(backend, key), key: key}, nil
}

// dial opens and verifies one connection pool. A misconfigured or unreachable
// server fails here, at boot, rather than at the first write.
func dial(backend, target string) (*sql.DB, error) {
	switch backend {
	case BackendPostgres:
		db, err := sql.Open("pgx", target)
		if err != nil {
			return nil, fmt.Errorf("storedb: open postgres: %w", err)
		}
		db.SetMaxOpenConns(maxOpenPostgres)
		db.SetMaxIdleConns(2)
		db.SetConnMaxLifetime(30 * time.Minute)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("storedb: ping postgres: %w", err)
		}
		return db, nil
	default:
		// Pragmas belong in the DSN rather than a one-off Exec: a pooled
		// connection opened later would otherwise default to busy_timeout=0
		// and fail immediately on SQLITE_BUSY instead of waiting for the
		// writer.
		db, err := sql.Open("sqlite",
			target+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
		if err != nil {
			return nil, fmt.Errorf("storedb: open sqlite %s: %w", target, err)
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("storedb: open sqlite %s: %w", target, err)
		}
		return db, nil
	}
}

// displayTarget is how the store's location appears in a log line: the file
// path, or the DSN with its credentials removed. A store's address may be
// logged; its password must not be.
func displayTarget(backend, target string) string {
	if backend == BackendPostgres {
		return RedactDSN(target)
	}
	return target
}

// Backend reports which implementation this store is on.
func (d *DB) Backend() string { return d.backend }

// Target reports where the store lives, in a form safe to log.
func (d *DB) Target() string { return d.target }

// Display renders a store target for a log line or an error message: a
// PostgreSQL DSN loses its credentials, a SQLite path is already safe. Anything
// that logs a target — including the config layer, which holds one before any
// store is opened — goes through this, because a DSN carries a password and a
// log line does not get to keep it.
func Display(target string) string {
	if BackendFor(target) != BackendPostgres {
		return target
	}
	return RedactDSN(target)
}

// Close releases this store's hold on the pool. The pool closes with the last
// holder, so a store closing does not pull the connection out from under its
// neighbours.
func (d *DB) Close() error {
	poolMu.Lock()
	defer poolMu.Unlock()
	p, ok := pools[d.key]
	if !ok {
		return nil
	}
	p.refs--
	if p.refs > 0 {
		return nil
	}
	delete(pools, d.key)
	return p.db.Close()
}

// Exec runs a statement. Placeholders are written as "?".
func (d *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.db.ExecContext(ctx, d.bind(query), args...)
}

// Query runs a statement and returns its rows.
func (d *DB) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.db.QueryContext(ctx, d.bind(query), args...)
}

// QueryRow runs a statement expected to return at most one row.
func (d *DB) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return d.db.QueryRowContext(ctx, d.bind(query), args...)
}

// ExecDDL runs schema statements one at a time. A single Exec carrying several
// statements is not portable: the PostgreSQL driver's extended protocol refuses
// more than one command in a prepared statement, so a schema that worked on one
// backend would fail on boot on the other. One statement per call is boring and
// works everywhere.
func (d *DB) ExecDDL(ctx context.Context, statements ...string) error {
	for _, stmt := range statements {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := d.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("storedb: ddl %q: %w", firstLine(stmt), err)
		}
	}
	return nil
}

// EnsureColumn adds a column when an older database lacks it, and does nothing
// when it is already there. CREATE TABLE IF NOT EXISTS silently does nothing on
// an existing table, so a column introduced after a release needs this — and
// unlike a bare ALTER it can run on every boot.
//
// definition is the column's SQL type and default, e.g. "TEXT NOT NULL DEFAULT ”".
func (d *DB) EnsureColumn(ctx context.Context, table, column, definition string) error {
	has, err := d.HasColumn(ctx, table, column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	// The identifiers are not parameters (no backend takes one for DDL) and
	// come from this package's own callers, never from a request. The
	// definition is a caller-supplied SQL fragment for the same reason.
	if _, err := d.db.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)); err != nil {
		return fmt.Errorf("storedb: add %s.%s: %w", table, column, err)
	}
	return nil
}

// HasColumn reports whether a table has a column. The two backends keep their
// catalogues in different places — this is the one thing about a schema that
// cannot be written once — but both statements still bind through the wrapper,
// so nothing here depends on which placeholder syntax a driver wants.
func (d *DB) HasColumn(ctx context.Context, table, column string) (bool, error) {
	var n int
	var err error
	switch d.backend {
	case BackendPostgres:
		err = d.QueryRow(ctx, `
			SELECT COUNT(*) FROM information_schema.columns
			WHERE table_name = ? AND column_name = ?`, table, column).Scan(&n)
	default:
		err = d.QueryRow(ctx,
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	}
	if err != nil {
		return false, fmt.Errorf("storedb: look up %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}

// bind rewrites placeholders for the backend. SQLite takes "?" as written.
func (d *DB) bind(query string) string {
	if d.backend != BackendPostgres {
		return query
	}
	return rebind(query)
}

// rebind converts "?" placeholders to PostgreSQL's "$1", "$2", … It skips
// placeholders inside single-quoted string literals, where a "?" is data and
// not a parameter — a literal that got rewritten would silently change what the
// statement means. (Dollar-quoted strings are not used by this engine's
// statements and are not handled.)
func rebind(query string) string {
	if !strings.ContainsRune(query, '?') {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	inQuote := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			// A doubled quote inside a literal toggles twice, which lands back
			// where it started: the escape needs no special case.
			inQuote = !inQuote
			b.WriteByte(c)
		case c == '?' && !inQuote:
			n++
			fmt.Fprintf(&b, "$%d", n)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// RedactDSN strips credentials from a PostgreSQL DSN so it can be logged. It
// errs toward saying less: a DSN it cannot read loses everything after the
// scheme rather than risking a password in a log line.
func RedactDSN(dsn string) string {
	scheme := "postgres://"
	if i := strings.Index(dsn, "://"); i >= 0 {
		scheme = dsn[:i+3]
	}
	at := strings.LastIndex(dsn, "@")
	if at < len(scheme) { // no "@", or one inside the scheme: no userinfo to keep
		return dsn
	}
	return scheme + "***@" + dsn[at+1:]
}

// firstLine keeps a DDL error message to the statement's opening line rather
// than the whole CREATE TABLE.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}
