// Package pgvector implements the pgvector vector backend provider.
// It provides the "vector" feature via PostgreSQL + the pgvector extension.
//
// The connection string is the same as postgres. The provider requires
// the pgvector extension to be installed on the target database. Each logical
// table maps to a vector table named "vec_<table>".
package pgvector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"agentflow/internal/core/memory"
	// pgvector.Vector implements driver.Valuer, returning the vector as a
	// PostgreSQL text literal (e.g. "[1.0,2.0]"). The $n::vector casts below
	// accept that text representation.
	"github.com/pgvector/pgvector-go"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Provider implements memory.BackendProvider for "pgvector".
type Provider struct{}

func (Provider) Name() string { return "pgvector" }

func (Provider) Features() []string {
	return []string{"vector"}
}

// defaultDim matches the embedding size of OpenAI's text-embedding-ada-002
// family; most self-hosted models are smaller (e.g. nomic-embed-text: 768),
// so dim is configurable per backend.
const defaultDim = 1536

func (Provider) Open(config map[string]any) (memory.BackendHandle, error) {
	url, _ := config["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("pgvector: url is required")
	}
	dim := defaultDim
	switch v := config["dim"].(type) {
	case int:
		dim = v
	case int64:
		dim = int(v)
	case float64:
		dim = int(v)
	}
	if dim <= 0 {
		return nil, fmt.Errorf("pgvector: dim must be positive, got %d", dim)
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("pgvector: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pgvector: ping: %w", err)
	}
	h := &Handle{db: db, dim: dim}
	if err := h.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pgvector: migrate: %w", err)
	}
	return h, nil
}

type Handle struct {
	db  *sql.DB
	dim int
}

func (h *Handle) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Ensure the extension exists, then create a generic vector table.
	_, err := h.db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector;`)
	if err != nil {
		return fmt.Errorf("create extension vector: %w (is pgvector installed?)", err)
	}
	// The embedding column width is fixed at create time; changing dim on an
	// existing database requires a manual ALTER TABLE (documented in docs).
	// Two statements in one Exec, and a Sprintf'd dimension rather than a bind
	// parameter — deliberately. pgx uses the simple protocol only when a
	// statement carries no arguments, and the extended protocol refuses
	// multi-command text, so passing dim as an argument here would break the
	// migration rather than parameterize it.
	_, err = h.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS vec_kv (
			table_name TEXT NOT NULL,
			key        TEXT NOT NULL,
			value      JSONB NOT NULL,
			embedding  vector(%d),
			updated_at BIGINT NOT NULL,
			PRIMARY KEY (table_name, key)
		);
		CREATE INDEX IF NOT EXISTS idx_vec_kv_embedding ON vec_kv USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100);
	`, h.dim))
	return err
}

func (h *Handle) Put(table, key string, value any, opts memory.PutOpts) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	now := time.Now().Unix()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if len(opts.Vector) > 0 {
		if len(opts.Vector) != h.dim {
			return fmt.Errorf("pgvector: embedding has %d dimensions, backend configured for %d (set dim in the backend config to match the embedding model)", len(opts.Vector), h.dim)
		}
		_, err = h.db.ExecContext(ctx,
			`INSERT INTO vec_kv (table_name, key, value, embedding, updated_at) VALUES ($1, $2, $3, $4::vector, $5)
			 ON CONFLICT (table_name, key) DO UPDATE SET value = EXCLUDED.value, embedding = EXCLUDED.embedding, updated_at = EXCLUDED.updated_at`,
			table, key, b, pgvector.NewVector(opts.Vector), now)
		return err
	}
	// No embedding supplied: upsert the value and keep any existing
	// embedding for the key rather than NULLing it out.
	_, err = h.db.ExecContext(ctx,
		`INSERT INTO vec_kv (table_name, key, value, updated_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (table_name, key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
		table, key, b, now)
	return err
}

func (h *Handle) Get(table, key string) (any, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var raw []byte
	err := h.db.QueryRowContext(ctx,
		`SELECT value FROM vec_kv WHERE table_name = $1 AND key = $2`,
		table, key).Scan(&raw)
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
		`DELETE FROM vec_kv WHERE table_name = $1 AND key = $2`, table, key)
	return err
}

func (h *Handle) Query(table string, q memory.Query) (memory.Iterator, error) {
	if q.Kind != "vector" {
		return nil, fmt.Errorf("pgvector: only vector queries are supported")
	}
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("pgvector: vector query requires a query vector")
	}
	k := q.K
	if k <= 0 {
		k = 10
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := h.db.QueryContext(ctx,
		`SELECT key, value FROM vec_kv
		 WHERE table_name = $1 AND embedding IS NOT NULL
		 ORDER BY embedding <=> $2::vector
		 LIMIT $3`,
		table, pgvector.NewVector(q.Vector), k)
	if err != nil {
		return nil, err
	}
	// Drained here, while ctx is alive: the rows are bound to it and would be
	// closed by the deferred cancel before the caller could read them.
	recs, err := memory.DrainRows(rows, scanKV)
	if err != nil {
		return nil, err
	}
	return memory.NewSliceIterator(recs), nil
}

// scanKV reads the (key, JSON value) column pair both PostgreSQL drivers store.
func scanKV(rows *sql.Rows) (memory.Record, error) {
	var key, raw string
	if err := rows.Scan(&key, &raw); err != nil {
		return memory.Record{}, err
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return memory.Record{}, err
	}
	return memory.Record{Key: key, Value: v}, nil
}

func (h *Handle) GC(table string, window int) error { return nil }
func (h *Handle) Close() error                      { return h.db.Close() }
