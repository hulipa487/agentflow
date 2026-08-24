// Package sqlite implements the builtin:sqlite memory backend provider.
// It stores JSON values keyed by (table, key) with optional TTL, supports
// prefix and text search (via FTS5), and keeps recent insertion order for
// recency queries.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentflow/internal/core/memory"

	_ "modernc.org/sqlite"
)

// Provider implements memory.BackendProvider for "builtin:sqlite".
type Provider struct{}

func (Provider) Name() string { return "builtin:sqlite" }

func (Provider) Features() []string {
	return []string{"kv", "prefix_scan", "text_search", "ttl", "transaction"}
}

func (Provider) Open(config map[string]any) (memory.BackendHandle, error) {
	path := "./data/agentflow.db"
	if p, ok := config["path"].(string); ok && p != "" {
		path = p
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if err := ensureDir(abs); err != nil {
		return nil, err
	}
	// modernc.org/sqlite is a pure Go driver; register is automatic.
	db, err := sql.Open("sqlite", abs)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", abs, err)
	}
	// SQLite defaults for agentflow: WAL, busy timeout, foreign keys.
	if _, err := db.Exec(`
		PRAGMA journal_mode = WAL;
		PRAGMA busy_timeout = 5000;
		PRAGMA foreign_keys = ON;
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite pragma: %w", err)
	}
	h := &Handle{db: db, path: abs}
	if err := h.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate sqlite %s: %w", abs, err)
	}
	return h, nil
}

func ensureDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0750)
}

// Handle is an opened sqlite backend.
type Handle struct {
	db   *sql.DB
	path string
}

func (h *Handle) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := h.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS kv (
			table_name TEXT NOT NULL,
			key TEXT NOT NULL,
			value TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			expires_at INTEGER,
			PRIMARY KEY (table_name, key)
		);
		CREATE INDEX IF NOT EXISTS idx_kv_table_time ON kv(table_name, updated_at);
		CREATE INDEX IF NOT EXISTS idx_kv_expires ON kv(expires_at);

		CREATE VIRTUAL TABLE IF NOT EXISTS kv_fts USING fts5(
			table_name, key, value,
			content='kv',
			content_rowid='rowid'
		);
		CREATE TRIGGER IF NOT EXISTS kv_ai AFTER INSERT ON kv BEGIN
			INSERT INTO kv_fts(rowid, table_name, key, value)
			VALUES (new.rowid, new.table_name, new.key, new.value);
		END;
		CREATE TRIGGER IF NOT EXISTS kv_ad AFTER DELETE ON kv BEGIN
			INSERT INTO kv_fts(kv_fts, rowid, table_name, key, value)
			VALUES ('delete', old.rowid, old.table_name, old.key, old.value);
		END;
		CREATE TRIGGER IF NOT EXISTS kv_au AFTER UPDATE ON kv BEGIN
			INSERT INTO kv_fts(kv_fts, rowid, table_name, key, value)
			VALUES ('delete', old.rowid, old.table_name, old.key, old.value);
			INSERT INTO kv_fts(rowid, table_name, key, value)
			VALUES (new.rowid, new.table_name, new.key, new.value);
		END;
	`)
	return err
}

func (h *Handle) Close() error { return h.db.Close() }

func (h *Handle) Put(table, key string, value any, opts memory.PutOpts) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	now := time.Now().Unix()
	var expires *int64
	if opts.TTL > 0 {
		exp := now + int64(opts.TTL.Seconds())
		expires = &exp
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = h.db.ExecContext(ctx,
		`INSERT INTO kv (table_name, key, value, updated_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(table_name, key) DO UPDATE SET
		   value = excluded.value,
		   updated_at = excluded.updated_at,
		   expires_at = excluded.expires_at`,
		table, key, string(b), now, expires)
	return err
}

func (h *Handle) Get(table, key string) (any, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var raw string
	err := h.db.QueryRowContext(ctx,
		`SELECT value FROM kv WHERE table_name = ? AND key = ? AND (expires_at IS NULL OR expires_at > ?)`,
		table, key, time.Now().Unix()).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, true, err
	}
	return v, true, nil
}

func (h *Handle) Delete(table, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.db.ExecContext(ctx,
		`DELETE FROM kv WHERE table_name = ? AND key = ?`,
		table, key)
	return err
}

func (h *Handle) Query(table string, q memory.Query) (memory.Iterator, error) {
	switch q.Kind {
	case "prefix":
		return h.queryPrefix(table, q.Prefix)
	case "text":
		return h.queryText(table, q.Text, q.K)
	case "time_range":
		return h.queryTimeRange(table, q.From, q.To)
	case "all":
		return h.queryAll(table)
	case "vector":
		// SQLite does not support vector queries. Return an explicit error
		// rather than a silent empty iterator — silent empty results would
		// mask a missing vector backend as "no matches", which is unsafe for
		// recall. Use a vector-capable backend (builtin:pgvector) instead.
		return nil, fmt.Errorf("vector queries are not supported by builtin:sqlite; configure a vector-capable backend")
	default:
		return nil, fmt.Errorf("unsupported query kind %q", q.Kind)
	}
}

func (h *Handle) queryPrefix(table, prefix string) (memory.Iterator, error) {
	rows, err := h.db.Query(
		`SELECT key, value FROM kv
		 WHERE table_name = ? AND key LIKE ? AND (expires_at IS NULL OR expires_at > ?)
		 ORDER BY updated_at DESC`,
		table, prefix+"%", time.Now().Unix())
	if err != nil {
		return nil, err
	}
	return &rowsIter{rows: rows}, nil
}

func (h *Handle) queryText(table, text string, k int) (memory.Iterator, error) {
	quoted := quoteFTS(text)
	limit := k
	if limit <= 0 {
		limit = 20
	}
	rows, err := h.db.Query(
		`SELECT kv.key, kv.value FROM kv
		 JOIN kv_fts ON kv_fts.rowid = kv.rowid
		 WHERE kv_fts.table_name = ? AND kv_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		table, quoted, limit)
	if err != nil {
		return nil, err
	}
	return &rowsIter{rows: rows}, nil
}

func (h *Handle) queryTimeRange(table string, from, to time.Time) (memory.Iterator, error) {
	rows, err := h.db.Query(
		`SELECT key, value FROM kv
		 WHERE table_name = ? AND updated_at BETWEEN ? AND ?
		 ORDER BY updated_at DESC`,
		table, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	return &rowsIter{rows: rows}, nil
}

func (h *Handle) queryAll(table string) (memory.Iterator, error) {
	rows, err := h.db.Query(
		`SELECT key, value FROM kv
		 WHERE table_name = ? AND (expires_at IS NULL OR expires_at > ?)
		 ORDER BY updated_at DESC`,
		table, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	return &rowsIter{rows: rows}, nil
}

func quoteFTS(s string) string {
	parts := strings.Fields(s)
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, " ")
}

// GC deletes expired records and keeps only the most recent N rows per table.
func (h *Handle) GC(table string, window int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Delete expired.
	if _, err := h.db.ExecContext(ctx,
		`DELETE FROM kv WHERE expires_at IS NOT NULL AND expires_at <= ?`,
		time.Now().Unix()); err != nil {
		return err
	}
	// Delete rows beyond the per-table window.
	if window <= 0 {
		return nil
	}
	_, err := h.db.ExecContext(ctx, `
		DELETE FROM kv WHERE rowid IN (
		  SELECT rowid FROM (
		    SELECT rowid,
		           ROW_NUMBER() OVER (PARTITION BY table_name ORDER BY updated_at DESC) AS rn
		    FROM kv
		    WHERE table_name = ?
		  )
		  WHERE rn > ?
		)`, table, window)
	return err
}

type rowsIter struct {
	rows *sql.Rows
	rec  memory.Record
	err  error
}

func (it *rowsIter) Next() bool {
	if it.err != nil {
		return false
	}
	if !it.rows.Next() {
		it.err = it.rows.Err()
		return false
	}
	var key, raw string
	if err := it.rows.Scan(&key, &raw); err != nil {
		it.err = err
		return false
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		it.err = err
		return false
	}
	it.rec = memory.Record{Key: key, Value: v}
	return true
}

func (it *rowsIter) Record() memory.Record { return it.rec }

func (it *rowsIter) Err() error {
	if it.err != nil {
		return it.err
	}
	return it.rows.Err()
}
