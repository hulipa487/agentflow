package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func scanTestKV(rows *sql.Rows) (Record, error) {
	var key, raw string
	if err := rows.Scan(&key, &raw); err != nil {
		return Record{}, err
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return Record{}, err
	}
	return Record{Key: key, Value: v}, nil
}

// queryWithDeferredCancel reproduces the shape the SQL drivers had: the
// context is cancelled as this function returns, i.e. before the caller has
// read a single row. A driver that handed back a live *sql.Rows here would
// have it torn down underneath the caller.
func queryWithDeferredCancel(db *sql.DB, limit int) (Iterator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM kv LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	recs, err := DrainRows(rows, scanTestKV)
	if err != nil {
		return nil, err
	}
	return NewSliceIterator(recs), nil
}

// seedKV opens an in-memory SQLite database holding n records.
func seedKV(t *testing.T, n int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// One connection: an in-memory database is per-connection.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO kv (key, value) VALUES (?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("k%07d", i), fmt.Sprintf(`{"n":%d}`, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestDrainRowsSurvivesDeferredCancel is the regression test for the iterator
// lifetime bug: pgvector, postgres and mongodb all returned a live cursor from
// a function that deferred its cancel, so database/sql (and the mongo driver)
// closed the result before the caller read it. The symptom was intermittent —
// small result sets are usually buffered before the cancellation is observed,
// which is why every existing test passed — and mid-iteration it showed up as
// a truncated result plus context.Canceled from Err(), not as a Query error.
//
// The row count is large on purpose: a handful of rows can be buffered before
// the cancel is noticed, so a small fixture would pass against the bug.
func TestDrainRowsSurvivesDeferredCancel(t *testing.T) {
	const rows = 50000
	db := seedKV(t, rows)

	it, err := queryWithDeferredCancel(db, rows)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	n := 0
	for it.Next() {
		if rec := it.Record(); rec.Key == "" {
			t.Fatal("record with an empty key")
		}
		n++
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	if n != rows {
		t.Fatalf("read %d rows; want %d — a context cancelled before iteration truncates the result", n, rows)
	}
}

// TestDrainRowsReportsErrorsFromQuery: a failure in the scan surfaces from
// DrainRows itself, so the caller sees it where it already handles errors
// rather than discovering a short result mid-iteration.
func TestDrainRowsReportsErrorsFromQuery(t *testing.T) {
	db := seedKV(t, 3)

	rows, err := db.Query(`SELECT key, value FROM kv`)
	if err != nil {
		t.Fatal(err)
	}
	// Scanning two columns into one value is a scan error.
	_, err = DrainRows(rows, func(r *sql.Rows) (Record, error) {
		var key string
		if err := r.Scan(&key); err != nil {
			return Record{}, err
		}
		return Record{Key: key}, nil
	})
	if err == nil {
		t.Fatal("a scan failure must be returned, not swallowed")
	}
}

// TestSliceIteratorBounds: reading before the first Next must yield the zero
// Record rather than panicking. Nothing reads it in that position today, but a
// driver's iterator is not guaranteed to be read only after a true Next.
func TestSliceIteratorBounds(t *testing.T) {
	it := NewSliceIterator([]Record{{Key: "a"}})
	if rec := it.Record(); rec.Key != "" {
		t.Fatalf("Record before Next = %+v; want the zero Record", rec)
	}
	if !it.Next() {
		t.Fatal("expected one record")
	}
	if it.Record().Key != "a" {
		t.Fatalf("Record = %+v", it.Record())
	}
	if it.Next() {
		t.Fatal("expected exhaustion")
	}
	if it.Err() != nil {
		t.Fatal("an exhausted iterator has no error")
	}

	empty := NewSliceIterator(nil)
	if empty.Next() {
		t.Fatal("an empty iterator must report no records")
	}
	if rec := empty.Record(); rec.Key != "" {
		t.Fatalf("an empty iterator must yield the zero Record, got %+v", rec)
	}
}
