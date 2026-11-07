// Package postgres implements the postgres memory backend provider.
// It provides kv, prefix_scan, text_search and transaction features via a
// PostgreSQL server, and vector queries as well when the database has the
// pgvector extension — in which case the backend reports "vector" and serves
// it from the same table, rather than needing a second backend of provider
// pgvector.
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
	"github.com/pgvector/pgvector-go"
)

// Provider implements memory.BackendProvider for "postgres".
type Provider struct{}

func (Provider) Name() string { return "postgres" }

// Features is the static answer. An opened handle reports the real one via
// BackendFeatures, because vector support depends on the database.
func (Provider) Features() []string {
	return []string{"kv", "prefix_scan", "text_search", "transaction"}
}

// defaultDim matches pgvector's and the embedding model family's default.
const defaultDim = 1536

func (Provider) Open(config map[string]any) (memory.BackendHandle, error) {
	url, _ := config["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("postgres: url is required")
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
		return nil, fmt.Errorf("postgres: dim must be positive, got %d", dim)
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	h := &Handle{db: db, dim: dim}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Vector support is a property of the database, not of the provider. No
	// CREATE EXTENSION here: installing server extensions is an operator's
	// decision (and usually a DBA's privilege), so a database without one
	// simply reports the features it can honour.
	var vectorExt int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pg_extension WHERE extname = 'vector'`).Scan(&vectorExt); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: check vector extension: %w", err)
	}
	h.hasVector = vectorExt > 0
	if !h.hasVector {
		// A dimension only means something with vectors; an explicit one on a
		// database without the extension is a configuration mistake worth
		// saying out loud rather than silently ignoring.
		if _, set := config["dim"]; set {
			_ = db.Close()
			return nil, fmt.Errorf("postgres: dim is set but this database has no vector extension; create it (CREATE EXTENSION vector) or remove dim")
		}
	}
	if err := h.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: migrate: %w", err)
	}
	return h, nil
}

type Handle struct {
	db        *sql.DB
	dim       int
	hasVector bool
}

// BackendFeatures reports what this database can actually serve.
func (h *Handle) BackendFeatures() []string {
	features := []string{"kv", "prefix_scan", "text_search", "transaction"}
	if h.hasVector {
		features = append(features, "vector")
	}
	return features
}

func (h *Handle) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A single generic table keyed by (table_name, key). The value is JSONB.
	// A GIN index on value supports text_search. A btree on updated_at
	// supports prefix_scan ordering.
	//
	// The embedding column exists only on a vector-capable database. It is in
	// the CREATE for a new table and added with ALTER for an existing one, so
	// enabling pgvector later upgrades the same table rather than moving data.
	embedding := ""
	if h.hasVector {
		embedding = fmt.Sprintf(",\n\t\t\tembedding  vector(%d)", h.dim)
	}
	if _, err := h.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS store_kv (
			table_name TEXT NOT NULL,
			key        TEXT NOT NULL,
			value      JSONB NOT NULL,
			updated_at BIGINT NOT NULL,
			expires_at BIGINT%s,
			PRIMARY KEY (table_name, key)
		);
		CREATE INDEX IF NOT EXISTS idx_store_kv_table_time ON store_kv(table_name, updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_store_kv_expires ON store_kv(expires_at) WHERE expires_at IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_store_kv_value_gin ON store_kv USING gin(value);
	`, embedding)); err != nil {
		return err
	}
	if !h.hasVector {
		return nil
	}
	if _, err := h.db.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE store_kv ADD COLUMN IF NOT EXISTS embedding vector(%d)`, h.dim)); err != nil {
		return err
	}
	// ivfflat is the index pgvector recommends for cosine k-NN. Creating it on
	// an empty table is allowed; it becomes useful as rows arrive.
	_, err := h.db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_store_kv_embedding ON store_kv USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100)`)
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
	if len(opts.Vector) > 0 {
		if !h.hasVector {
			return fmt.Errorf("postgres: a vector was supplied but this database has no vector extension; create it or store without a vector")
		}
		if len(opts.Vector) != h.dim {
			return fmt.Errorf("postgres: embedding has %d dimensions, backend configured for %d (set dim in the backend config to match the embedding model)", len(opts.Vector), h.dim)
		}
		_, err = h.db.ExecContext(ctx,
			`INSERT INTO store_kv (table_name, key, value, embedding, updated_at, expires_at)
			 VALUES ($1, $2, $3, $4::vector, $5, $6)
			 ON CONFLICT (table_name, key) DO UPDATE SET
			   value = EXCLUDED.value,
			   embedding = EXCLUDED.embedding,
			   updated_at = EXCLUDED.updated_at,
			   expires_at = EXCLUDED.expires_at`,
			table, key, b, pgvector.NewVector(opts.Vector), now, expires)
		return err
	}
	if h.hasVector {
		// No embedding supplied: keep any existing one rather than NULLing it.
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
	case "vector":
		return h.queryVector(table, q)
	default:
		return nil, fmt.Errorf("postgres: unsupported query kind %q", q.Kind)
	}
}

func (h *Handle) queryVector(table string, q memory.Query) (memory.Iterator, error) {
	if !h.hasVector {
		return nil, fmt.Errorf("postgres: vector queries require the vector extension in this database; create it (CREATE EXTENSION vector) or configure a vector-capable backend")
	}
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("postgres: vector query requires a query vector")
	}
	if len(q.Vector) != h.dim {
		return nil, fmt.Errorf("postgres: query vector has %d dimensions, backend configured for %d", len(q.Vector), h.dim)
	}
	k := q.K
	if k <= 0 {
		k = 10
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := h.db.QueryContext(ctx,
		`SELECT key, value FROM store_kv
		 WHERE table_name = $1 AND embedding IS NOT NULL
		   AND (expires_at IS NULL OR expires_at > $4)
		 ORDER BY embedding <=> $2::vector
		 LIMIT $3`,
		table, pgvector.NewVector(q.Vector), k, time.Now().Unix())
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
	// Drained here, while ctx is alive: the rows are bound to it and would be
	// closed by the deferred cancel before the caller could read them.
	recs, err := memory.DrainRows(rows, scanKV)
	if err != nil {
		return nil, err
	}
	return memory.NewSliceIterator(recs), nil
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
		   AND (expires_at IS NULL OR expires_at > $4)
		 ORDER BY updated_at DESC LIMIT $3`,
		table, text, k, time.Now().Unix())
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
	// Drained here, while ctx is alive: the rows are bound to it and would be
	// closed by the deferred cancel before the caller could read them.
	recs, err := memory.DrainRows(rows, scanKV)
	if err != nil {
		return nil, err
	}
	return memory.NewSliceIterator(recs), nil
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

// scanKV reads the (key, JSON value) column pair the store_kv table holds.
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
