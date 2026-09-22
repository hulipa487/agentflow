package storedb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scheme decides the backend; everything else is a file path.
func TestBackendFor(t *testing.T) {
	for target, want := range map[string]string{
		"postgres://u:p@h:5432/db": BackendPostgres,
		"postgresql://u:p@h/db":    BackendPostgres,
		"./data/agentflow.db":      BackendSQLite,
		"":                         BackendSQLite,
		"postgres-ish.db":          BackendSQLite,
	} {
		if got := BackendFor(target); got != want {
			t.Errorf("BackendFor(%q) = %q, want %q", target, got, want)
		}
	}
}

// A target names its backend with a scheme, or is a bare path meaning the
// first allowed backend. The parser is the one place that decides, so the
// differences between callers live in what they allow rather than in how they
// read a string.
func TestParseTarget(t *testing.T) {
	store := []string{BackendSQLite, BackendPostgres}
	for _, tt := range []struct {
		name    string
		raw     string
		allowed []string
		backend string
		address string
		wantErr string
	}{
		{name: "a postgres dsn", raw: "postgres://u:p@h:5432/db", allowed: store,
			backend: BackendPostgres, address: "postgres://u:p@h:5432/db"},
		{name: "the postgresql spelling", raw: "postgresql://h/db", allowed: store,
			backend: BackendPostgres, address: "postgresql://h/db"},
		{name: "an explicit sqlite file", raw: "sqlite://./data/x.db", allowed: store,
			backend: BackendSQLite, address: "./data/x.db"},
		{name: "a bare path is the local form", raw: "./data/x.db", allowed: store,
			backend: BackendSQLite, address: "./data/x.db"},
		{name: "a drive letter is not a scheme", raw: `C:\data\x.db`, allowed: store,
			backend: BackendSQLite, address: `C:\data\x.db`},
		{name: "the log plane takes a directory", raw: "./data/log", allowed: []string{BackendFile},
			backend: BackendFile, address: "./data/log"},
		{name: "the log plane spells it file://", raw: "file://./data/log", allowed: []string{BackendFile},
			backend: BackendFile, address: "./data/log"},
		{name: "a store will not take the log plane's backend", raw: "file://./data/log", allowed: store,
			wantErr: "this setting takes sqlite, postgres"},
		{name: "the log plane will not take a store's", raw: "sqlite://./data/log", allowed: []string{BackendFile},
			wantErr: "this setting takes file"},
		{name: "an unknown scheme", raw: "mongodb://host/db", allowed: store,
			wantErr: "does not have (mongodb)"},
		{name: "another one", raw: "scylla://host:9042", allowed: store,
			wantErr: "does not have (scylla)"},
		{name: "no address", raw: "sqlite://", allowed: store, wantErr: "has no address"},
		{name: "nothing at all", raw: "   ", allowed: store, wantErr: "empty target"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTarget(tt.raw, tt.allowed...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected an error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Backend != tt.backend || got.Address != tt.address {
				t.Fatalf("parsed %q as %s %q, want %s %q", tt.raw, got.Backend, got.Address, tt.backend, tt.address)
			}
		})
	}
}

// The trap the parser exists for: a scheme the engine does not have used to be
// classified as a local file path, so a Mongo DSN became a directory named
// "mongodb:" on one operating system and an invalid-path failure on another.
// It is now refused, by name, before anything tries to open it.
func TestOpenRefusesAnUnknownScheme(t *testing.T) {
	dir := t.TempDir()
	for _, target := range []string{
		"mongodb://host/db",
		"scylla://host:9042",
		"mysql://host/db",
		filepath.Join(dir, "x.db") + "://odd",
	} {
		_, err := Open(target)
		if err == nil {
			t.Fatalf("Open(%q) succeeded, want a refusal", target)
		}
		if !strings.Contains(err.Error(), "does not have") {
			t.Fatalf("Open(%q) = %v, want it to name the unknown scheme", target, err)
		}
	}
	// Nothing was created on the way to that refusal.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused target created %d entries: %v", len(entries), entries)
	}
}

// Placeholders are renumbered in order, and a "?" that is data stays data: a
// literal rewritten by accident would change what the statement means.
func TestRebind(t *testing.T) {
	cases := map[string]string{
		"SELECT 1":                                   "SELECT 1",
		"SELECT a FROM t WHERE x = ?":                "SELECT a FROM t WHERE x = $1",
		"INSERT INTO t (a, b) VALUES (?, ?)":         "INSERT INTO t (a, b) VALUES ($1, $2)",
		"SELECT a FROM t WHERE x = ? AND y = ?":      "SELECT a FROM t WHERE x = $1 AND y = $2",
		"SELECT '?' FROM t WHERE x = ?":              "SELECT '?' FROM t WHERE x = $1",
		"SELECT '?', ? FROM t":                       "SELECT '?', $1 FROM t",
		"SELECT a FROM t WHERE x = 'lit''?''eral'":   "SELECT a FROM t WHERE x = 'lit''?''eral'",
		"SELECT 'a?b' , ? , 'c?d'":                   "SELECT 'a?b' , $1 , 'c?d'",
		"UPDATE t SET a = ? WHERE b = ? AND c = '?'": "UPDATE t SET a = $1 WHERE b = $2 AND c = '?'",
	}
	for in, want := range cases {
		if got := rebind(in); got != want {
			t.Errorf("rebind(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// A DSN is logged at boot; its password must not travel with it.
func TestRedactDSN(t *testing.T) {
	cases := map[string]string{
		"postgres://user:s3cret@db.internal:5432/agentflow": "postgres://***@db.internal:5432/agentflow",
		"postgres://db.internal:5432/agentflow":             "postgres://db.internal:5432/agentflow",
	}
	for in, want := range cases {
		if got := RedactDSN(in); got != want {
			t.Errorf("RedactDSN(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"postgres://user:s3cret@db/agentflow", "postgresql://u:p@h/d"} {
		got := RedactDSN(in)
		if strings.Contains(got, "s3cret") || strings.Contains(got, ":p@") {
			t.Errorf("RedactDSN leaked credentials: %q", got)
		}
	}
}

// Anything that logs a store target uses Display: a DSN carries a password, a
// file path carries nothing, and a path that merely looks like a DSN (or
// contains an "@") must survive intact.
func TestDisplay(t *testing.T) {
	cases := map[string]string{
		"postgres://u:s3cret@h:5432/db":         "postgres://***@h:5432/db",
		"postgresql://o:pw@ep-x.neon.tech/db":   "postgresql://***@ep-x.neon.tech/db",
		"./data/identity.db":                    "./data/identity.db",
		"":                                      "",
		"postgres-ish.db":                       "postgres-ish.db",
		`C:\Users\oscar@work\identity.db`:       `C:\Users\oscar@work\identity.db`,
		"/srv/agentflow/data/credentials.db":    "/srv/agentflow/data/credentials.db",
		"postgres://db.internal:5432/agentflow": "postgres://db.internal:5432/agentflow",
	}
	for in, want := range cases {
		if got := Display(in); got != want {
			t.Errorf("Display(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{
		"postgres://user:s3cret@db/agentflow",
		"postgresql://neondb_owner:npg_secret@ep-x-pooler.ap-southeast-2.aws.neon.tech/neondb?sslmode=require",
	} {
		if got := Display(in); strings.Contains(got, "s3cret") || strings.Contains(got, "npg_secret") {
			t.Errorf("Display leaked a password: %q", got)
		}
	}
}

// A store opens, migrates and round-trips rows with "?" placeholders.
func TestOpenSQLiteAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "store.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = d.Close() }()

	if d.Backend() != BackendSQLite {
		t.Errorf("backend = %q, want sqlite", d.Backend())
	}
	// The parent directory is created for a file target, and the reported
	// target is absolute so a log line names a real file.
	if !filepath.IsAbs(d.Target()) {
		t.Errorf("target should be absolute, got %q", d.Target())
	}

	ctx := context.Background()
	if err := d.ExecDDL(ctx,
		`CREATE TABLE IF NOT EXISTS t (k TEXT PRIMARY KEY, v BIGINT NOT NULL);`,
		``, // blank statements are skipped, not an error
		`CREATE INDEX IF NOT EXISTS t_v ON t (v);`,
	); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO t (k, v) VALUES (?, ?)`, "a", 1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO t (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, "a", 2); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var v int64
	if err := d.QueryRow(ctx, `SELECT v FROM t WHERE k = ?`, "a").Scan(&v); err != nil {
		t.Fatalf("query row: %v", err)
	}
	if v != 2 {
		t.Errorf("upsert did not replace: v = %d", v)
	}
	rows, err := d.Query(ctx, `SELECT k FROM t WHERE v = ?`, 2)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if n != 1 {
		t.Errorf("expected one row, got %d", n)
	}

	// A bad statement reports which one failed rather than the whole schema.
	if err := d.ExecDDL(ctx, `CREATE TABLE t (k TEXT);`); err == nil {
		t.Error("a failing DDL statement must report an error")
	} else if !strings.Contains(err.Error(), "CREATE TABLE t") {
		t.Errorf("ddl error should name the statement, got: %v", err)
	}
}

// Stores on one target share a pool: in a fleet every store points at the same
// server, and one pool per store is how a fleet exhausts its connection limit.
// Closing one store must not pull the pool from under another.
func TestPoolSharing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	a, err := Open(path)
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	b, err := Open(path)
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	// A third handle on a differently-spelled path to the same file still
	// shares, because the target is canonicalized.
	c, err := Open(filepath.Join(filepath.Dir(path), ".", filepath.Base(path)))
	if err != nil {
		t.Fatalf("open c: %v", err)
	}
	if a.db != b.db || a.db != c.db {
		t.Fatal("stores on one target must share a pool")
	}

	if err := a.Close(); err != nil {
		t.Fatalf("close a: %v", err)
	}
	// b and c still hold the pool, so it stays open and usable.
	if _, err := b.Exec(context.Background(), `CREATE TABLE IF NOT EXISTS t (k TEXT)`); err != nil {
		t.Fatalf("pool closed early: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close b: %v", err)
	}
	if _, err := c.Exec(context.Background(), `SELECT 1`); err != nil {
		t.Fatalf("pool closed with a holder left: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close c: %v", err)
	}

	poolMu.Lock()
	_, still := pools[c.key]
	poolMu.Unlock()
	if still {
		t.Error("the pool should be dropped once its last holder closes")
	}
	// Closing twice is a no-op rather than a panic on a missing entry.
	if err := c.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

// A file target that cannot be created fails at open, at boot, rather than at
// the first write.
func TestOpenRejectsEmptyTarget(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("an empty target must not open a store")
	}
}
