package logfile_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agentflow/internal/core/runtime"
	"agentflow/internal/drivers/logfile"
)

// The two backends of the log plane have to answer the same questions the same
// way, or "move the journal to a log plane" would quietly change what an
// operator sees. This runs one scenario against the SQL store and against the
// append log and compares the answers.
//
// It lives here rather than beside the store's own contract test because the
// dependency only runs one way: the log plane implements the store's interfaces,
// so a test inside the runtime package cannot import it.

// journalScenario records a fixed set of entries and returns the answers both
// backends must agree on.
func journalScenario(t *testing.T, j runtime.Journal) map[string][]string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	records := []runtime.JournalEntry{
		{ID: "m1", Ts: now.Add(-3 * time.Minute).Unix(), Direction: "in", Status: "routed",
			Channel: "webhook", Sender: "alice", UserUUID: "user:1", Agent: "bot", Text: "one"},
		{ID: "m2", Ts: now.Add(-2 * time.Minute).Unix(), Direction: "out", Status: "delivered",
			Channel: "webhook", Sender: "user:1", UserUUID: "user:1", Agent: "bot",
			SessionID: "bot|webhook:alice", Text: "two"},
		{ID: "m3", Ts: now.Add(-time.Minute).Unix(), Direction: "in", Status: "routed",
			Channel: "webhook", Sender: "bob", UserUUID: "user:2", Agent: "bot", Text: "three"},
	}
	for _, e := range records {
		if err := j.RecordMessage(ctx, e); err != nil {
			t.Fatalf("record %s: %v", e.ID, err)
		}
	}

	ids := func(entries []runtime.JournalEntry) []string {
		out := []string{}
		for _, e := range entries {
			out = append(out, e.ID)
		}
		return out
	}
	answers := map[string][]string{}
	for name, f := range map[string]runtime.JournalFilter{
		"all":        {},
		"by user":    {UserUUID: "user:1"},
		"by dir":     {Direction: "out"},
		"by window":  {Since: now.Add(-150 * time.Second).Unix(), Until: now.Add(-30 * time.Second).Unix()},
		"limited":    {Limit: 2},
		"by session": {SessionID: "bot|webhook:alice"},
	} {
		got, err := j.ListMessages(ctx, f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		answers[name] = ids(got)
	}
	return answers
}

func TestJournalBackendsAgree(t *testing.T) {
	sql, err := runtime.OpenSQLite(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = sql.Close() })
	file, err := logfile.Open(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })

	want := journalScenario(t, sql)
	got := journalScenario(t, file)
	for name, w := range want {
		g := got[name]
		if len(g) != len(w) {
			t.Fatalf("%s: the log plane returned %v, the store %v", name, g, w)
		}
		for i := range w {
			if g[i] != w[i] {
				t.Fatalf("%s: %v vs %v (order matters: newest first)", name, g, w)
			}
		}
	}

	// A window that begins and ends inside one day is the case the day-file
	// listing has to get right: it may not skip the file it needs.
	if len(want["by window"]) == 0 {
		t.Fatal("the scenario itself is wrong: no rows in the window")
	}
}

// The same for the per-call detail.
func TestEventBackendsAgree(t *testing.T) {
	sql, err := runtime.OpenSQLite(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = sql.Close() })
	file, err := logfile.Open(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })

	now := time.Now().Truncate(time.Second)
	// One successful call, one the provider refused: the refused one counts as
	// an invocation and bills nothing, on both backends.
	records := []runtime.UsageRecord{
		{UserID: "user:1", Agent: "bot", Model: "gpt", Kind: "chat", Input: 100, Output: 20, OK: true, At: now.Add(-2 * time.Minute)},
		{UserID: "user:1", Agent: "bot", Model: "gpt", Kind: "chat", Input: 50, Output: 10, OK: false, At: now.Add(-time.Minute)},
	}
	for _, backend := range []struct {
		name   string
		ledger runtime.Ledger
		events runtime.EventLog
	}{
		{"store", sql, sql},
		{"log plane", nil, file},
	} {
		t.Run(backend.name, func(t *testing.T) {
			for _, rec := range records {
				if backend.ledger != nil {
					if err := backend.ledger.RecordUsage(rec, true); err != nil {
						t.Fatalf("record usage: %v", err)
					}
					continue
				}
				if err := backend.events.AppendEvent(rec.UserID, runtime.EventFrom(rec)); err != nil {
					t.Fatalf("append event: %v", err)
				}
			}
			got, err := backend.events.UsageEvents("user:1", now.Add(-time.Hour), 50)
			if err != nil {
				t.Fatalf("read events: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("events = %d, want 2", len(got))
			}
			// Newest first; the refused call has zero tokens and OK false.
			if got[0].OK || got[0].Input != 0 {
				t.Fatalf("the refused call was not normalised: %+v", got[0])
			}
			if !got[1].OK || got[1].Input != 100 || got[1].Output != 20 {
				t.Fatalf("the successful call lost its totals: %+v", got[1])
			}
			if got[1].Kind != "chat" || got[1].Model != "gpt" || got[1].Agent != "bot" {
				t.Fatalf("attribution lost: %+v", got[1])
			}
		})
	}
}
