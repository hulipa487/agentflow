// Package sqlite implements the sqlite memory backend provider.
// It stores JSON values keyed by (table, key) with optional TTL, supports
// prefix and text search (via FTS5), and keeps recent insertion order for
// recency queries.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentflow/internal/core/memory"

	_ "modernc.org/sqlite"
)

// Provider implements memory.BackendProvider for "sqlite".
type Provider struct{}

func (Provider) Name() string { return "sqlite" }

func (Provider) Features() []string {
	return []string{"kv", "prefix_scan", "text_search", "ttl", "transaction", "vector"}
}

// defaultVectorScanLimit caps a brute-force vector scan. SQLite has no vector
// index (the pure-Go driver cannot load C extensions), so a query reads every
// embedding in the table; 50k float32 rows is the point past which that stops
// being a table scan and starts being an outage.
const defaultVectorScanLimit = 50_000

func (Provider) Open(config map[string]any) (memory.BackendHandle, error) {
	path := "./data/agentflow.db"
	if p, ok := config["path"].(string); ok && p != "" {
		path = p
	}
	scanLimit := defaultVectorScanLimit
	if n, ok := intConfig(config, "vector_scan_limit"); ok && n > 0 {
		scanLimit = n
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
	h := &Handle{db: db, path: abs, vectorScanLimit: scanLimit}
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
	db              *sql.DB
	path            string
	vectorScanLimit int
}

// intConfig reads an optional positive integer from a backend config map.
func intConfig(config map[string]any, key string) (int, bool) {
	switch v := config[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	default:
		return 0, false
	}
}

func (h *Handle) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := h.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS kv (
			table_name TEXT NOT NULL,
			key TEXT NOT NULL,
			value TEXT NOT NULL,
			embedding BLOB,
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
	if err != nil {
		return err
	}
	// A database created before vectors existed needs the column added;
	// CREATE TABLE IF NOT EXISTS silently does nothing on an existing table.
	return h.addColumnIfMissing(ctx, "kv", "embedding", "BLOB")
}

// addColumnIfMissing is this driver's migration path for a column introduced
// after the table shipped. SQLite has no ADD COLUMN IF NOT EXISTS, so the
// catalogue is asked first.
func (h *Handle) addColumnIfMissing(ctx context.Context, table, column, typ string) error {
	var n int
	if err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := h.db.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, typ))
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
	if len(opts.Vector) > 0 {
		_, err = h.db.ExecContext(ctx,
			`INSERT INTO kv (table_name, key, value, embedding, updated_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(table_name, key) DO UPDATE SET
			   value = excluded.value,
			   embedding = excluded.embedding,
			   updated_at = excluded.updated_at,
			   expires_at = excluded.expires_at`,
			table, key, string(b), encodeVector(opts.Vector), now, expires)
		return err
	}
	// No embedding supplied: keep any existing one rather than NULLing it out.
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

// encodeVector stores float32 embeddings little-endian in a BLOB. float32 is
// the wire and pgvector representation; keeping it halves the scan's memory
// without changing a recall result anyone can observe.
func encodeVector(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// decodeVector reads a BLOB written by encodeVector. A length that is not a
// multiple of four is a corrupt row, not a shorter vector.
func decodeVector(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("embedding blob has %d bytes, not a multiple of 4", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
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
		return h.queryVector(table, q)
	default:
		return nil, fmt.Errorf("unsupported query kind %q", q.Kind)
	}
}

// queryVector is a brute-force cosine scan. SQLite has no vector index the
// pure-Go driver can use, so the honest implementation reads every embedding
// in the table and ranks in Go. The row cap is what keeps that from turning a
// large deployment into a stalled process; beyond it the error says what to do.
func (h *Handle) queryVector(table string, q memory.Query) (memory.Iterator, error) {
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("sqlite: vector query requires a query vector")
	}
	k := q.K
	if k <= 0 {
		k = 10
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	now := time.Now().Unix()
	var n int
	if err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kv
		 WHERE table_name = ? AND embedding IS NOT NULL AND (expires_at IS NULL OR expires_at > ?)`,
		table, now).Scan(&n); err != nil {
		return nil, err
	}
	if n > h.vectorScanLimit {
		return nil, fmt.Errorf("sqlite: vector scan would read %d rows, above the limit of %d; raise vector_scan_limit on the backend or use pgvector", n, h.vectorScanLimit)
	}
	rows, err := h.db.QueryContext(ctx,
		`SELECT key, value, embedding FROM kv
		 WHERE table_name = ? AND embedding IS NOT NULL AND (expires_at IS NULL OR expires_at > ?)`,
		table, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type hit struct {
		rec   memory.Record
		score float64
	}
	hits := make([]hit, 0, n)
	for rows.Next() {
		var key, raw string
		var blob []byte
		if err := rows.Scan(&key, &raw, &blob); err != nil {
			return nil, err
		}
		vec, err := decodeVector(blob)
		if err != nil {
			return nil, err
		}
		if len(vec) != len(q.Vector) {
			return nil, fmt.Errorf("sqlite: embedding for key %q has %d dimensions, query has %d (the embedding model changed); re-embed or clear the store", key, len(vec), len(q.Vector))
		}
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, err
		}
		hits = append(hits, hit{rec: memory.Record{Key: key, Value: v}, score: cosine(q.Vector, vec)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > k {
		hits = hits[:k]
	}
	recs := make([]memory.Record, len(hits))
	for i, h := range hits {
		recs[i] = h.rec
	}
	return memory.NewSliceIterator(recs), nil
}

// cosine is the similarity pgvector's cosine operator computes, in Go.
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		// A zero vector has no direction, so cosine is undefined. Rank it as
		// the least similar rather than as 0, which would put it above any
		// negative similarity; pgvector's NaN ordering puts it last too.
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
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

// ExportRows implements memory.RowExporter.
func (h *Handle) ExportRows(table string) ([]memory.ExportRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rows, err := h.db.QueryContext(ctx,
		`SELECT key, value, embedding, expires_at FROM kv
		 WHERE table_name = ? AND (expires_at IS NULL OR expires_at > ?)
		 ORDER BY updated_at, key`,
		table, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.ExportRecord
	for rows.Next() {
		var key, raw string
		var emb []byte
		var exp sql.NullInt64
		if err := rows.Scan(&key, &raw, &emb, &exp); err != nil {
			return nil, err
		}
		rec := memory.ExportRecord{Key: key}
		if err := json.Unmarshal([]byte(raw), &rec.Value); err != nil {
			return nil, fmt.Errorf("sqlite: %s/%s: %w", table, key, err)
		}
		if len(emb) > 0 {
			if rec.Vector, err = decodeVector(emb); err != nil {
				return nil, fmt.Errorf("sqlite: %s/%s: %w", table, key, err)
			}
		}
		if exp.Valid {
			rec.ExpiresAt = time.Unix(exp.Int64, 0)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
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

// rowsIter streams a SQLite result set lazily, unlike the PostgreSQL driver,
// which drains into memory.DrainRows before returning.
//
// That asymmetry is deliberate, not an oversight: those drivers bound their
// query to a context with a timeout and cancelled it on return, so a cursor
// handed back live would be torn down before the caller read it. This driver
// calls db.Query with no context in every query path below, so there is no
// cancel to outlive and holding the rows open is safe. If a context is ever
// added here, switch these paths to memory.DrainRows rather than adding a
// cancel — the bug that pattern causes is intermittent and reads as a
// cold-start failure.
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
