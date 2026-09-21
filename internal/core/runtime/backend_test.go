package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// AGENTFLOW_TEST_POSTGRES names the server the Postgres half of this suite runs
// against, e.g.
//
//	AGENTFLOW_TEST_POSTGRES='postgres://user:pass@localhost:5432/agentflow?sslmode=disable' \
//	  go test ./internal/core/runtime/
//
// Unset, only the SQLite backend runs — which is the default everywhere, and
// the reason this suite is written to be re-runnable against a shared server:
// every case tags its data with a per-run id rather than assuming empty tables.
var postgresDSN = os.Getenv("AGENTFLOW_TEST_POSTGRES")

// forEachBackend runs fn against every available runtime store.
func forEachBackend(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		s, err := OpenSQLite(filepath.Join(t.TempDir(), "runtime.db"))
		if err != nil {
			t.Fatalf("open sqlite store: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		fn(t, s)
	})
	t.Run("postgres", func(t *testing.T) {
		if postgresDSN == "" {
			t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
		}
		s, err := OpenPostgres(postgresDSN)
		if err != nil {
			t.Fatalf("open postgres store: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		fn(t, s)
	})
}

// tag is a per-run identifier: re-running against a shared server must not let
// one run's rows satisfy (or break) another's assertions.
func tag(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "t" + hex.EncodeToString(b) + "_"
}

// TestStoreContract runs the same behavioural contract against both backends.
// Anything asserted here has to mean the same thing on a shared server as it
// does in a local file, because the fleet depends on that.
func TestStoreContract(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id := tag(t)
		user := id + "u1"

		// --- journal ---------------------------------------------------------
		old := time.Now().Add(-48 * time.Hour).Unix()
		if err := s.RecordMessage(ctx, JournalEntry{
			ID: id + "m1", Ts: old, Direction: "in", Status: "routed",
			Channel: "telegram", Sender: "user:" + user, UserUUID: user, Text: "old",
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
		// A row with every optional field empty must round-trip, not fail.
		if err := s.RecordMessage(ctx, JournalEntry{
			ID: id + "m2", Ts: time.Now().Unix(), Direction: "in", Status: "routed",
			UserUUID: user, Text: "new",
			Provenance: map[string]any{"kind": "user"},
		}); err != nil {
			t.Fatalf("record sparse: %v", err)
		}
		rows, err := s.ListMessages(ctx, JournalFilter{UserUUID: user})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("expected 2 rows for %s, got %d (%+v)", user, len(rows), rows)
		}
		if rows[0].Text != "new" {
			t.Fatalf("newest first expected, got %+v", rows[0])
		}
		if rows[1].Provenance != nil && len(rows[1].Provenance) != 0 {
			t.Fatalf("row without provenance should read back empty: %+v", rows[1])
		}
		if rows[0].Provenance["kind"] != "user" {
			t.Fatalf("provenance did not round-trip: %+v", rows[0])
		}
		if n, err := s.PruneMessages(ctx, time.Now().Add(-24*time.Hour)); err != nil || n < 1 {
			t.Fatalf("prune should remove at least the old row: n=%d err=%v", n, err)
		}
		rows, _ = s.ListMessages(ctx, JournalFilter{UserUUID: user})
		if len(rows) != 1 || rows[0].Text != "new" {
			t.Fatalf("prune kept the wrong rows: %+v", rows)
		}

		// --- key/value rows --------------------------------------------------
		if err := s.PutRow(ctx, "t|"+user+"|p|a.txt", `{"path":"a.txt"}`, time.Time{}); err != nil {
			t.Fatalf("put row: %v", err)
		}
		if err := s.PutRow(ctx, "t|"+user+"|p|b.txt", `{"path":"b.txt"}`, time.Time{}); err != nil {
			t.Fatalf("put row: %v", err)
		}
		// A prefix scan must not leak across scopes. "u_1" is the trap: "_" is a
		// LIKE wildcard, so a naive pattern would also match this neighbour.
		neighbour := "t|" + id + "ux1|p|c.txt"
		if err := s.PutRow(ctx, neighbour, `{}`, time.Time{}); err != nil {
			t.Fatalf("put neighbour: %v", err)
		}
		scanned, err := s.ListRows(ctx, "t|"+user+"|p|")
		if err != nil {
			t.Fatalf("list rows: %v", err)
		}
		if len(scanned) != 2 {
			t.Fatalf("prefix scan must match only its own scope, got %+v", scanned)
		}
		if got, ok, err := s.GetRow(ctx, "t|"+user+"|p|a.txt"); err != nil || !ok || got.Value == "" {
			t.Fatalf("get row: ok=%v err=%v", ok, err)
		}
		// Upsert replaces rather than duplicating.
		if err := s.PutRow(ctx, "t|"+user+"|p|a.txt", `{"path":"a.txt","v":2}`, time.Time{}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if scanned, _ := s.ListRows(ctx, "t|"+user+"|p|"); len(scanned) != 2 {
			t.Fatalf("upsert duplicated a row: %+v", scanned)
		}
		// An expired row is missing on read and reclaimed by the sweep.
		if err := s.PutRow(ctx, "s|"+user+"|tmp", `{}`, time.Now().Add(-time.Minute)); err != nil {
			t.Fatalf("put expired: %v", err)
		}
		if _, ok, err := s.GetRow(ctx, "s|"+user+"|tmp"); err != nil || ok {
			t.Fatalf("an expired row must read as missing: ok=%v err=%v", ok, err)
		}
		if err := s.PutRow(ctx, "s|"+user+"|tmp2", `{}`, time.Now().Add(-time.Minute)); err != nil {
			t.Fatalf("put expired: %v", err)
		}
		if n, err := s.SweepExpired(ctx); err != nil || n < 1 {
			t.Fatalf("sweep should reclaim expired rows: n=%d err=%v", n, err)
		}
		if err := s.DeleteRow(ctx, "t|"+user+"|p|b.txt"); err != nil {
			t.Fatalf("delete row: %v", err)
		}
		if _, ok, _ := s.GetRow(ctx, "t|"+user+"|p|b.txt"); ok {
			t.Fatal("delete did not remove the row")
		}

		// --- usage ledger ----------------------------------------------------
		day := DayKey(time.Now())
		at := time.Now()
		for i := 0; i < 2; i++ {
			if err := s.RecordUsage(UsageRecord{
				UserID: user, Agent: "bot", Model: "m", Kind: "chat",
				Input: 100, Output: 20, Cached: 64, OK: true, At: at,
			}, false); err != nil {
				t.Fatalf("record usage: %v", err)
			}
		}
		if err := s.RecordUsage(UsageRecord{
			UserID: user, Agent: "bot", Model: "m", Kind: "chat",
			Input: 999, Output: 999, OK: false, At: at,
		}, false); err != nil {
			t.Fatalf("record failure: %v", err)
		}
		totals, err := s.UsageForDay(user, day)
		if err != nil {
			t.Fatalf("usage: %v", err)
		}
		if totals.Input != 200 || totals.Output != 40 || totals.Cached != 128 {
			t.Fatalf("the rollup must fold, not replace: %+v", totals)
		}
		if totals.Calls != 3 || totals.Failed != 1 {
			t.Fatalf("a failed call counts as an attempt: %+v", totals)
		}
		if totals.Billable() != 240 {
			t.Fatalf("billable = input+output: %+v", totals)
		}
		// The per-call detail is opt-in.
		if evs, _ := s.UsageEvents(user, at.Add(-time.Hour), 10); len(evs) != 0 {
			t.Fatalf("no event rows expected when the detail log is off: %+v", evs)
		}
		if err := s.RecordUsage(UsageRecord{
			UserID: user, Agent: "bot", Model: "m", Kind: "stream",
			Input: 5, Output: 1, Cached: 3, OK: true, At: at,
		}, true); err != nil {
			t.Fatalf("record with event: %v", err)
		}
		evs, err := s.UsageEvents(user, at.Add(-time.Hour), 10)
		if err != nil || len(evs) != 1 || evs[0].Kind != "stream" || evs[0].Cached != 3 {
			t.Fatalf("event row: %+v err=%v", evs, err)
		}
		// History covers today, and another user's spend stays out of it.
		hist, err := s.UsageHistory(user, 7)
		if err != nil || len(hist) == 0 {
			t.Fatalf("history: %+v err=%v", hist, err)
		}
		for _, r := range hist {
			if r.UserID != user {
				t.Fatalf("history leaked another user: %+v", r)
			}
		}
		// The operator view is per-day and includes this run's row.
		daily, err := s.UsageDaily(day)
		if err != nil {
			t.Fatalf("daily: %v", err)
		}
		found := false
		for _, r := range daily {
			if r.UserID == user {
				found = true
			}
		}
		if !found {
			t.Fatalf("daily view is missing this run's row")
		}
		// The per-agent rollup sums one agent's rows across every user — the
		// figure a deployment-wide agent budget is measured against. On a shared
		// server other runs may have contributed, so this is a lower bound.
		agentTotals, err := s.UsageForAgentDay("bot", day)
		if err != nil {
			t.Fatalf("agent usage: %v", err)
		}
		if agentTotals.Input < 200 || agentTotals.Output < 40 {
			t.Fatalf("the per-agent rollup is missing this run's rows: %+v", agentTotals)
		}
		// An agent nobody called has nothing.
		if none, err := s.UsageForAgentDay(id+"nobody", day); err != nil || none.Calls != 0 {
			t.Fatalf("an unknown agent should have no usage: %+v err=%v", none, err)
		}
		// Pruning by age leaves today alone.
		if err := s.PruneUsage(time.Now().AddDate(0, 0, -30)); err != nil {
			t.Fatalf("prune usage: %v", err)
		}
		if totals, _ := s.UsageForDay(user, day); totals.Calls == 0 {
			t.Fatal("pruning 30 days back must not touch today")
		}
	})
}

// The Postgres store must refuse a target that is not a DSN rather than opening
// a database named after a file path.
func TestOpenPostgresRejectsNonDSN(t *testing.T) {
	if _, err := OpenPostgres("./data/agentflow.db"); err == nil {
		t.Fatal("a file path must not be accepted as a postgres DSN")
	}
}

// BackendFor drives the constructor choice, so its mapping is worth pinning.
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

// A DSN is logged at boot; its password must not travel with it.
func TestRedactDSN(t *testing.T) {
	cases := map[string]string{
		"postgres://user:s3cret@db.internal:5432/agentflow": "postgres://***@db.internal:5432/agentflow",
		"postgres://db.internal:5432/agentflow":             "postgres://db.internal:5432/agentflow",
	}
	for in, want := range cases {
		if got := redactDSN(in); got != want {
			t.Errorf("redactDSN(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"postgres://user:s3cret@db/agentflow", "postgresql://u:p@h/d"} {
		got := redactDSN(in)
		if strings.Contains(got, "s3cret") || strings.Contains(got, ":p@") {
			t.Errorf("redactDSN leaked credentials: %q", got)
		}
	}
}
