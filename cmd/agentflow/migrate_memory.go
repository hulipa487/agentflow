package main

// Memory rows are the one part of a store `migrate store` cannot copy as-is.
// Each backend keeps them in its own table with its own types: the sqlite
// memory driver writes table kv with a TEXT value and a float32 BLOB vector;
// the postgres driver writes store_kv with a JSONB value and a vector(dim)
// column, and the pgvector provider a separate vec_kv. Moving a single
// deployment to a regional cell is exactly a move from the first to the
// second, so this file converts rows between them rather than copying columns.
//
// What it keeps is what recall depends on: updated_at (recency orders history
// by it), expires_at (a row's TTL survives the move instead of becoming
// permanent) and the embedding (semantic recall finds the row without a
// re-embed). Rows already expired are left behind.

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/core/storedb"
	pgmem "agentflow/internal/drivers/postgres"
	sqlitemem "agentflow/internal/drivers/sqlite"
)

// memoryTable is the pseudo-table name `-tables` accepts for the memory rows.
const memoryTable = "memory"

// memoryRow is one memory entry in backend-neutral form.
type memoryRow struct {
	table, key, value string
	vector            []float32
	updatedAt         int64
	expiresAt         *int64
}

// memoryTableFor is the table a store's memory driver keeps rows in.
func memoryTableFor(db *storedb.DB) string {
	if db.Backend() == storedb.BackendPostgres {
		return "store_kv"
	}
	return "kv"
}

// migrateMemory copies the memory rows of src into dst. It returns how many
// rows it copied; a source without a memory table copies nothing.
func migrateMemory(ctx context.Context, src, dst *storedb.DB, srcTarget, dstTarget string, appendRows bool, stdout io.Writer) (int64, error) {
	srcTable := memoryTableFor(src)
	count, err := tableCount(ctx, src, srcTable)
	if err != nil {
		fmt.Fprintf(stdout, "skip  %-16s (not in source)\n", memoryTable)
		return 0, nil
	}
	if count == 0 {
		fmt.Fprintf(stdout, "skip  %-16s (empty)\n", memoryTable)
		return 0, nil
	}

	dim, err := sourceVectorDim(ctx, src, srcTable)
	if err != nil {
		return 0, err
	}
	if err := ensureMemorySchema(dst, dstTarget, dim); err != nil {
		return 0, err
	}
	dstTable := memoryTableFor(dst)
	if !appendRows {
		existing, err := tableCount(ctx, dst, dstTable)
		if err != nil {
			return 0, err
		}
		if existing > 0 {
			return 0, fmt.Errorf("destination table %q already has %d rows; pass -append to copy into it", dstTable, existing)
		}
	}
	dstVectors, err := dst.HasColumn(ctx, dstTable, "embedding")
	if err != nil {
		return 0, err
	}
	if dim > 0 && !dstVectors {
		return 0, fmt.Errorf("the source has embeddings but the destination database has no vector extension; run CREATE EXTENSION vector on it first, or semantic recall loses every migrated row")
	}

	rows, err := readMemoryRows(ctx, src, srcTable)
	if err != nil {
		return 0, err
	}
	n, err := writeMemoryRows(ctx, dst, dstTable, dstVectors, rows)
	if err != nil {
		return n, err
	}
	fmt.Fprintf(stdout, "copy  %-16s %d rows (%s → %s)\n", memoryTable, n, srcTable, dstTable)
	if src.Backend() == storedb.BackendPostgres {
		if c, err := tableCount(ctx, src, "vec_kv"); err == nil && c > 0 {
			fmt.Fprintf(stdout, "note: %d rows in vec_kv (provider pgvector) were not copied; point that backend at the destination server or copy the table with pg_dump\n", c)
		}
	}
	return n, nil
}

// sourceVectorDim reports the embedding width the source's rows use, or 0 when
// none carries one. The destination's vector column is created at this width.
func sourceVectorDim(ctx context.Context, src *storedb.DB, table string) (int, error) {
	has, err := src.HasColumn(ctx, table, "embedding")
	if err != nil || !has {
		return 0, err
	}
	var q string
	if src.Backend() == storedb.BackendPostgres {
		q = "SELECT vector_dims(embedding) FROM " + table + " WHERE embedding IS NOT NULL LIMIT 1"
	} else {
		q = "SELECT length(embedding) / 4 FROM " + table + " WHERE embedding IS NOT NULL AND length(embedding) > 0 LIMIT 1"
	}
	var dim int
	if err := src.QueryRow(ctx, q).Scan(&dim); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return dim, nil
}

// ensureMemorySchema creates the destination's memory table by opening its
// memory driver, the same migration a boot runs, so the rows land in a table
// the engine accepts.
func ensureMemorySchema(dst *storedb.DB, target string, dim int) error {
	t, err := storedb.ParseTarget(target, storedb.BackendSQLite, storedb.BackendPostgres)
	if err != nil {
		return err
	}
	if dst.Backend() == storedb.BackendPostgres {
		cfg := map[string]any{"url": t.Address}
		if dim > 0 {
			cfg["dim"] = dim
		}
		h, err := pgmem.Provider{}.Open(cfg)
		if err != nil {
			return fmt.Errorf("destination memory store: %w", err)
		}
		return h.Close()
	}
	h, err := sqlitemem.Provider{}.Open(map[string]any{"path": t.Address})
	if err != nil {
		return fmt.Errorf("destination memory store: %w", err)
	}
	return h.Close()
}

// readMemoryRows reads every unexpired row. Memory is small next to the
// journal — it is bounded by what agents chose to remember — so the rows are
// read before the destination is written rather than streamed across two
// connections that may be one SQLite file's lock.
func readMemoryRows(ctx context.Context, src *storedb.DB, table string) ([]memoryRow, error) {
	hasVec, err := src.HasColumn(ctx, table, "embedding")
	if err != nil {
		return nil, err
	}
	pg := src.Backend() == storedb.BackendPostgres
	value, embedding := "value", "NULL"
	if pg {
		value = "value::text"
	}
	if hasVec {
		embedding = "embedding"
		if pg {
			embedding = "embedding::text"
		}
	}
	rs, err := src.Query(ctx,
		"SELECT table_name, key, "+value+", "+embedding+", updated_at, expires_at FROM "+table+
			" WHERE expires_at IS NULL OR expires_at > ? ORDER BY table_name, updated_at, key",
		time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []memoryRow
	for rs.Next() {
		var r memoryRow
		var emb any
		var exp *int64
		if err := rs.Scan(&r.table, &r.key, &r.value, &emb, &r.updatedAt, &exp); err != nil {
			return nil, err
		}
		r.expiresAt = exp
		if r.vector, err = decodeAnyVector(emb); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", r.table, r.key, err)
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// writeMemoryRows inserts the rows in batches.
func writeMemoryRows(ctx context.Context, dst *storedb.DB, table string, vectors bool, rows []memoryRow) (int64, error) {
	pg := dst.Backend() == storedb.BackendPostgres
	value, embedding := "?", "?"
	if pg {
		value, embedding = "CAST(? AS jsonb)", "CAST(? AS vector)"
	}
	cols := "table_name, key, value, updated_at, expires_at"
	vals := "?, ?, " + value + ", ?, ?"
	if vectors {
		cols += ", embedding"
		vals += ", " + embedding
	}
	insert := "INSERT INTO " + table + " (" + cols + ") VALUES (" + vals + ")"

	const batch = 1000
	var copied int64
	for start := 0; start < len(rows); start += batch {
		end := min(start+batch, len(rows))
		tx, err := dst.BeginTx(ctx, nil)
		if err != nil {
			return copied, err
		}
		for _, r := range rows[start:end] {
			args := []any{r.table, r.key, r.value, r.updatedAt, r.expiresAt}
			if vectors {
				var v any
				if len(r.vector) > 0 {
					if pg {
						v = vectorLiteral(r.vector)
					} else {
						v = encodeFloat32s(r.vector)
					}
				}
				args = append(args, v)
			}
			if _, err := tx.Exec(ctx, insert, args...); err != nil {
				_ = tx.Rollback()
				return copied, fmt.Errorf("%s/%s: %w", r.table, r.key, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return copied, err
		}
		copied += int64(end - start)
	}
	return copied, nil
}

// decodeAnyVector reads an embedding as either driver stores it: the sqlite
// driver's little-endian float32 BLOB, or pgvector's text form "[1,2,3]".
func decodeAnyVector(v any) ([]float32, error) {
	switch e := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		if len(e) > 0 && e[0] == '[' {
			return parseVectorLiteral(string(e))
		}
		if len(e)%4 != 0 {
			return nil, fmt.Errorf("embedding blob has %d bytes, not a multiple of 4", len(e))
		}
		out := make([]float32, len(e)/4)
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(e[i*4:]))
		}
		return out, nil
	case string:
		return parseVectorLiteral(e)
	default:
		return nil, fmt.Errorf("embedding has unexpected type %T", v)
	}
}

func parseVectorLiteral(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("embedding: %w", err)
		}
		out[i] = float32(f)
	}
	return out, nil
}

func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// encodeFloat32s is the sqlite memory driver's embedding encoding.
func encodeFloat32s(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// serialTables are the tables whose id a PostgreSQL sequence assigns. A copy
// writes the ids explicitly, which does not advance the sequence, so the next
// row the engine inserts would collide with a migrated one.
var serialTables = map[string]string{"usage_events": "id"}

// advanceSequence moves a copied table's id sequence past its highest row.
// SQLite needs nothing: AUTOINCREMENT follows the largest id inserted.
func advanceSequence(ctx context.Context, dst *storedb.DB, table string) error {
	col, ok := serialTables[table]
	if !ok || dst.Backend() != storedb.BackendPostgres {
		return nil
	}
	_, err := dst.Exec(ctx, fmt.Sprintf(
		`SELECT setval(pg_get_serial_sequence('%s', '%s'), COALESCE((SELECT MAX(%s) FROM %s), 0) + 1, false)`,
		table, col, col, table))
	return err
}
