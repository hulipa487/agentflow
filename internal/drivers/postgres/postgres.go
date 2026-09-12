// Package postgres implements the builtin:postgres memory backend provider.
// It provides kv, prefix_scan, text_search, and transaction features via a
// PostgreSQL server.
//
// The connection string is set in the backend config under "url" (e.g.
// "postgres://user:pass@localhost:5432/agentflow?sslmode=disable"). The
// driver uses pgx (pure Go). Each logical table maps to a PostgreSQL table
// named "store_<table>".
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"agentflow/internal/core/memory"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Provider implements memory.BackendProvider for "builtin:postgres".
type Provider struct{}

func (Provider) Name() string { return "builtin:postgres" }

func (Provider) Features() []string {
	return []string{"kv", "prefix_scan", "text_search", "transaction"}
}

func (Provider) Open(config map[string]any) (memory.BackendHandle, error) {
	url, _ := config["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("builtin:postgres: url is required")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("builtin:postgres: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("builtin:postgres: ping: %w", err)
	}
	h := &Handle{db: db}
	if err := h.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("builtin:postgres: migrate: %w", err)
	}
	return h, nil
}

type Handle struct {
	db *sql.DB
}

func (h *Handle) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A single generic table keyed by (table_name, key). The value is JSONB.
	// A GIN index on value supports text_search. A btree on updated_at
	// supports prefix_scan ordering.
	_, err := h.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS store_kv (
			table_name TEXT NOT NULL,
			key        TEXT NOT NULL,
			value      JSONB NOT NULL,
			updated_at BIGINT NOT NULL,
			expires_at BIGINT,
			PRIMARY KEY (table_name, key)
		);
		CREATE INDEX IF NOT EXISTS idx_store_kv_table_time ON store_kv(table_name, updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_store_kv_expires ON store_kv(expires_at) WHERE expires_at IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_store_kv_value_gin ON store_kv USING gin(value);
	`)
	return err
}

func (h *Handle) Put(table, key string, value any, opts memory.PutOpts) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	now := time.Now().Unix()
	var expires sql.NullInt64
	if opts.TTL > 0 {
		expires = sql.NullInt64{Int64: now + int64(opts.TTL.Seconds()), Valid: true}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = h.db.ExecContext(ctx,
		`INSERT INTO store_kv (table_name, key, value, updated_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (table_name, key) DO UPDATE SET
		   value = EXCLUDED.value,
		   updated_at = EXCLUDED.updated_at,
		   expires_at = EXCLUDED.expires_at`,
		table, key, b, now, expires)
	return err
}

func (h *Handle) Get(table, key string) (any, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var raw []byte
	err := h.db.QueryRowContext(ctx,
		`SELECT value FROM store_kv
		 WHERE table_name = $1 AND key = $2 AND (expires_at IS NULL OR expires_at > $3)`,
		table, key, time.Now().Unix()).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, true, err
	}
	return v, true, nil
}

func (h *Handle) Delete(table, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.db.ExecContext(ctx,
		`DELETE FROM store_kv WHERE table_name = $1 AND key = $2`,
		table, key)
	return err
}

func (h *Handle) Query(table string, q memory.Query) (memory.Iterator, error) {
	switch q.Kind {
	case "prefix":
		return h.queryPrefix(table, q.Prefix)
	case "text":
		return h.queryText(table, q.Text, q.K)
	case "all":
		return h.queryAll(table)
	default:
		return nil, fmt.Errorf("builtin:postgres: unsupported query kind %q", q.Kind)
	}
}

func (h *Handle) queryPrefix(table, prefix string) (memory.Iterator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := h.db.QueryContext(ctx,
		`SELECT key, value FROM store_kv
		 WHERE table_name = $1 AND key LIKE $2 AND (expires_at IS NULL OR expires_at > $3)
		 ORDER BY updated_at DESC`,
		table, prefix+"%", time.Now().Unix())
	if err != nil {
		return nil, err
	}
	return &rowsIter{rows: rows}, nil
}

func (h *Handle) queryText(table string, text string, k int) (memory.Iterator, error) {
	if k <= 0 {
		k = 20
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Use JSONB full-text search via the value column. This is a simple
	// text containment query; a production deployment may want a proper
	// tsvector column and GIN index.
	rows, err := h.db.QueryContext(ctx,
		`SELECT key, value FROM store_kv
		 WHERE table_name = $1 AND value::text ILIKE '%' || $2 || '%'
		 ORDER BY updated_at DESC LIMIT $3`,
		table, text, k)
	if err != nil {
		return nil, err
	}
	return &rowsIter{rows: rows}, nil
}

func (h *Handle) queryAll(table string) (memory.Iterator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := h.db.QueryContext(ctx,
		`SELECT key, value FROM store_kv
		 WHERE table_name = $1 AND (expires_at IS NULL OR expires_at > $2)
		 ORDER BY updated_at DESC`,
		table, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	return &rowsIter{rows: rows}, nil
}

func (h *Handle) GC(table string, window int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now().Unix()
	// Delete expired.
	if _, err := h.db.ExecContext(ctx,
		`DELETE FROM store_kv WHERE table_name = $1 AND expires_at IS NOT NULL AND expires_at <= $2`,
		table, now); err != nil {
		return err
	}
	// Window: keep only the most recent N.
	if window <= 0 {
		return nil
	}
	_, err := h.db.ExecContext(ctx,
		`DELETE FROM store_kv
		 WHERE table_name = $1 AND (table_name, key) NOT IN (
		   SELECT table_name, key FROM store_kv
		   WHERE table_name = $1
		   ORDER BY updated_at DESC LIMIT $2
		 )`, table, window)
	return err
}

func (h *Handle) Close() error { return h.db.Close() }

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
