package logfile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/runtime"
)

func openLog(t *testing.T) *Log {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// entry builds a journal entry at a given time.
func entry(id, user, direction string, at time.Time) runtime.JournalEntry {
	return runtime.JournalEntry{
		ID: id, Ts: at.Unix(), Direction: direction, Status: "routed",
		Channel: "webhook", Sender: "alice", UserUUID: user, Text: "text of " + id,
	}
}

func ids(entries []runtime.JournalEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

func TestJournalRoundTripAndFiltering(t *testing.T) {
	l := openLog(t)
	ctx := context.Background()
	now := time.Now()

	for _, e := range []runtime.JournalEntry{
		entry("m1", "user:1", "in", now.Add(-3*time.Minute)),
		entry("m2", "user:1", "out", now.Add(-2*time.Minute)),
		entry("m3", "user:2", "in", now.Add(-time.Minute)),
	} {
		if err := l.RecordMessage(ctx, e); err != nil {
			t.Fatalf("record %s: %v", e.ID, err)
		}
	}

	// Newest first, and the fields survive the round trip.
	all, err := l.ListMessages(ctx, runtime.JournalFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(all); len(got) != 3 || got[0] != "m3" || got[2] != "m1" {
		t.Fatalf("newest first = %v", got)
	}
	if all[0].Text != "text of m3" || all[0].Channel != "webhook" || all[0].Status != "routed" {
		t.Fatalf("entry did not round-trip: %+v", all[0])
	}

	// The filters the store's WHERE clause applies.
	mine, err := l.ListMessages(ctx, runtime.JournalFilter{UserUUID: "user:1"})
	if err != nil || len(mine) != 2 {
		t.Fatalf("by user = %v, err=%v", ids(mine), err)
	}
	out, err := l.ListMessages(ctx, runtime.JournalFilter{Direction: "out"})
	if err != nil || len(out) != 1 || out[0].ID != "m2" {
		t.Fatalf("by direction = %v, err=%v", ids(out), err)
	}
	window, err := l.ListMessages(ctx, runtime.JournalFilter{
		Since: now.Add(-150 * time.Second).Unix(),
		Until: now.Add(-30 * time.Second).Unix(),
	})
	if err != nil || len(window) != 2 {
		t.Fatalf("by window = %v, err=%v", ids(window), err)
	}
	if n, err := l.JournalRowCount(ctx); err != nil || n != 3 {
		t.Fatalf("row count = %d, err=%v", n, err)
	}
}

// A record belongs to the day it happened, not the day it was written: that is
// what makes retention by date mean what it says.
func TestRecordsLandInTheirOwnDay(t *testing.T) {
	l := openLog(t)
	ctx := context.Background()
	now := time.Now()
	old := now.AddDate(0, 0, -3)

	if err := l.RecordMessage(ctx, entry("m-old", "user:1", "in", old)); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordMessage(ctx, entry("m-new", "user:1", "in", now)); err != nil {
		t.Fatal(err)
	}

	files, err := os.ReadDir(l.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name())
	}
	wantOld := "journal-" + runtime.DayKey(old) + ".jsonl"
	wantNew := "journal-" + runtime.DayKey(now) + ".jsonl"
	if len(names) != 2 || !contains(names, wantOld) || !contains(names, wantNew) {
		t.Fatalf("day files = %v, want %s and %s", names, wantOld, wantNew)
	}

	// A window that excludes the old day does not open its file at all — the
	// answer is the same either way, which is the point of the day in the name.
	recent, err := l.ListMessages(ctx, runtime.JournalFilter{Since: now.Add(-time.Hour).Unix()})
	if err != nil || len(recent) != 1 || recent[0].ID != "m-new" {
		t.Fatalf("recent window = %v, err=%v", ids(recent), err)
	}
}

// Retention is whole days: the day a cutoff falls inside is kept, and the prune
// reports what it removed the way the SQL backends report rows.
func TestPruneRemovesWholeDaysAndKeepsToday(t *testing.T) {
	l := openLog(t)
	ctx := context.Background()
	now := time.Now()

	oldDay := now.AddDate(0, 0, -10)
	for i, id := range []string{"old-1", "old-2"} {
		if err := l.RecordMessage(ctx, entry(id, "user:1", "in", oldDay.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.RecordMessage(ctx, entry("today", "user:1", "in", now)); err != nil {
		t.Fatal(err)
	}

	n, err := l.PruneMessages(ctx, now.AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Fatalf("pruned %d entries, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(l.Dir(), "journal-"+runtime.DayKey(oldDay)+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("the old day file survived: %v", err)
	}
	left, err := l.ListMessages(ctx, runtime.JournalFilter{})
	if err != nil || len(left) != 1 || left[0].ID != "today" {
		t.Fatalf("after the prune = %v, err=%v", ids(left), err)
	}

	// The handle for a pruned day is closed, so a late record for that day
	// recreates the file rather than writing into a deleted one.
	if err := l.RecordMessage(ctx, entry("late", "user:1", "in", oldDay)); err != nil {
		t.Fatalf("record after prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.Dir(), "journal-"+runtime.DayKey(oldDay)+".jsonl")); err != nil {
		t.Fatalf("the file was not recreated: %v", err)
	}
}

// A line that will not decode is skipped rather than making the day unreadable:
// a crash mid-write leaves a truncated tail, and the records before it are
// still the audit trail.
func TestATruncatedTailIsSkipped(t *testing.T) {
	l := openLog(t)
	ctx := context.Background()
	now := time.Now()
	if err := l.RecordMessage(ctx, entry("good", "user:1", "in", now)); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(l.Dir(), "journal-"+runtime.DayKey(now)+".jsonl")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"trunc","ts":`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := l.ListMessages(ctx, runtime.JournalFilter{})
	if err != nil {
		t.Fatalf("a truncated tail must not fail the read: %v", err)
	}
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("entries = %v, want just the good one", ids(got))
	}
}

// The limit takes the newest rows across days, not the newest file's rows.
func TestLimitTakesTheNewestAcrossDays(t *testing.T) {
	l := openLog(t)
	ctx := context.Background()
	now := time.Now()
	for i := 0; i < 3; i++ {
		at := now.AddDate(0, 0, -1).Add(time.Duration(i) * time.Minute)
		if err := l.RecordMessage(ctx, entry("yesterday-"+string(rune('a'+i)), "user:1", "in", at)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.RecordMessage(ctx, entry("today", "user:1", "in", now)); err != nil {
		t.Fatal(err)
	}

	got, err := l.ListMessages(ctx, runtime.JournalFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "today" || got[1].ID != "yesterday-c" {
		t.Fatalf("limit = %v, want today then the newest of yesterday", ids(got))
	}
}

func TestEventLog(t *testing.T) {
	l := openLog(t)
	now := time.Now()

	for i, ev := range []runtime.UsageEvent{
		{Ts: now.Add(-2 * time.Minute).Unix(), Kind: "chat", Model: "gpt", Agent: "bot",
			UsageTotals: runtime.UsageTotals{Input: 10, Output: 5}, OK: true},
		{Ts: now.Add(-time.Minute).Unix(), Kind: "chat", Model: "gpt", Agent: "bot",
			UsageTotals: runtime.UsageTotals{Input: 20, Output: 7}, OK: true},
	} {
		if err := l.AppendEvent("user:1", ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// Another user's call, which the read must not return.
	if err := l.AppendEvent("user:2", runtime.UsageEvent{Ts: now.Unix(), Kind: "chat",
		UsageTotals: runtime.UsageTotals{Input: 99}, OK: true}); err != nil {
		t.Fatal(err)
	}

	got, err := l.UsageEvents("user:1", now.Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2", len(got))
	}
	// Newest first, and the totals survive the round trip.
	if got[0].Input != 20 || got[1].Input != 10 {
		t.Fatalf("order or values wrong: %+v", got)
	}

	// An old day file goes, the day being written stays.
	old := now.AddDate(0, 0, -6)
	if err := l.AppendEvent("user:1", runtime.UsageEvent{Ts: old.Unix(), Kind: "chat", OK: true}); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(l.Dir(), "events-"+runtime.DayKey(old)+".jsonl")
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("the old day was not written: %v", err)
	}
	if _, err := l.PruneEvents(now.AddDate(0, 0, -2)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatalf("the old events file survived a 2-day retention: %v", err)
	}

	// Retention drops whole days, and the file with it.
	if _, err := l.PruneEvents(now.AddDate(0, 0, 7)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	// A cutoff in the future would take today's file too — except that today is
	// never pruned, so the day being written survives.
	if got, err := l.UsageEvents("user:1", now.Add(-time.Hour), 50); err != nil || len(got) != 2 {
		t.Fatalf("today's events were pruned: %d, err=%v", len(got), err)
	}
	if _, err := l.PruneEvents(now.AddDate(0, 0, -7)); err != nil {
		t.Fatalf("prune (old cutoff): %v", err)
	}
}

// An empty directory is not an error: a deployment that has written nothing yet
// asks the same questions and gets empty answers.
func TestEmptyLog(t *testing.T) {
	l := openLog(t)
	ctx := context.Background()
	if got, err := l.ListMessages(ctx, runtime.JournalFilter{}); err != nil || len(got) != 0 {
		t.Fatalf("empty log = %v, err=%v", ids(got), err)
	}
	if n, err := l.JournalRowCount(ctx); err != nil || n != 0 {
		t.Fatalf("empty count = %d, err=%v", n, err)
	}
	if n, err := l.PruneMessages(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("prune of nothing = %d, err=%v", n, err)
	}
	if got, err := l.UsageEvents("user:1", time.Now().Add(-time.Hour), 10); err != nil || len(got) != 0 {
		t.Fatalf("empty events = %d, err=%v", len(got), err)
	}
}

// Writes after Close are refused rather than silently dropped.
func TestCloseRefusesFurtherWrites(t *testing.T) {
	l := openLog(t)
	ctx := context.Background()
	if err := l.RecordMessage(ctx, entry("m1", "user:1", "in", time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordMessage(ctx, entry("m2", "user:1", "in", time.Now())); err == nil {
		t.Fatal("a write after Close must be an error")
	}
	if err := l.AppendEvent("user:1", runtime.UsageEvent{Ts: time.Now().Unix()}); err == nil {
		t.Fatal("an event after Close must be an error")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
