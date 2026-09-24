package storedb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEnsureColumnIsIdempotent: EnsureColumn guards the ALTER with a catalogue
// lookup so it can run on every boot. The lookup and the ALTER are two steps,
// and the ALTER was a bare ADD COLUMN — which turns a lost race into a boot
// failure instead of a no-op. PostgreSQL now gets IF NOT EXISTS; on SQLite the
// guard is what makes the repeat safe, and this pins that.
func TestEnsureColumnIsIdempotent(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if err := d.ExecDDL(ctx, `CREATE TABLE t (k TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := d.EnsureColumn(ctx, "t", "v", "TEXT NOT NULL DEFAULT ''"); err != nil {
			t.Fatalf("EnsureColumn call %d: %v", i, err)
		}
	}
	if has, err := d.HasColumn(ctx, "t", "v"); err != nil || !has {
		t.Fatalf("column not added: has=%v err=%v", has, err)
	}
	// A column that is already there is a no-op, not an error.
	if err := d.EnsureColumn(ctx, "t", "k", "TEXT"); err != nil {
		t.Fatalf("ensuring an existing column: %v", err)
	}
	// And the lookup distinguishes absent from present, which is the whole
	// reason the guard is worth having.
	if absent, err := d.HasColumn(ctx, "t", "nope"); err != nil || absent {
		t.Fatalf("HasColumn reported a column that does not exist: has=%v err=%v", absent, err)
	}
}

// TestEnsureColumnPostgresSchemaScope covers the other half of the change, and
// needs a real server. The catalogue lookup spans every schema on the
// search_path, so without the schema filter a same-named table elsewhere
// answered yes and the caller skipped an ALTER its own table needed — a missing
// column discovered later, far from the cause.
func TestEnsureColumnPostgresSchemaScope(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
	}
	ctx := context.Background()
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	suffix := time.Now().UnixNano()
	table := fmt.Sprintf("af_ensure_%d", suffix)
	t.Cleanup(func() {
		_, _ = d.Exec(ctx, "DROP TABLE IF EXISTS "+table)
	})

	if _, err := d.Exec(ctx, "CREATE TABLE "+table+" (k TEXT)"); err != nil {
		t.Fatal(err)
	}
	// A same-named table in ANOTHER schema, already carrying the column.
	if _, err := d.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS af_other_schema"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, "CREATE TABLE af_other_schema."+table+" (k TEXT, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(ctx, "DROP TABLE IF EXISTS af_other_schema."+table) })

	has, err := d.HasColumn(ctx, table, "v")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("HasColumn answered for another schema's table; the ALTER this schema needs would be skipped")
	}

	// The real path: the column is added here, and repeating it is a no-op
	// rather than a duplicate-column failure.
	for i := 0; i < 2; i++ {
		if err := d.EnsureColumn(ctx, table, "v", "TEXT NOT NULL DEFAULT ''"); err != nil {
			t.Fatalf("EnsureColumn call %d: %v", i, err)
		}
	}
	if has, err := d.HasColumn(ctx, table, "v"); err != nil || !has {
		t.Fatalf("column not added in this schema: has=%v err=%v", has, err)
	}
}
