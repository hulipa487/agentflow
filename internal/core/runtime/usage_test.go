package runtime

import (
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestUsageRollupAccumulates: the daily rollup is the hot path for both quotas
// and dashboards, so repeated calls must fold into one row rather than
// replacing it.
func TestUsageRollupAccumulates(t *testing.T) {
	s := testStore(t)
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	day := DayKey(at)

	for i := 0; i < 2; i++ {
		if err := s.RecordUsage(UsageRecord{
			UserID: "u_1", Agent: "bot", Model: "m", Kind: "chat",
			Input: 100, Output: 20, Cached: 64, CacheWrite: 8, Reasoning: 5,
			OK: true, At: at,
		}, false); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	got, err := s.UsageForDay("u_1", day)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if got.Input != 200 || got.Output != 40 || got.Cached != 128 || got.CacheWrite != 16 || got.Reasoning != 10 {
		t.Fatalf("rollup did not accumulate: %+v", got)
	}
	if got.Calls != 2 || got.Failed != 0 {
		t.Fatalf("call counts: %+v", got)
	}
	if got.Billable() != 240 {
		t.Fatalf("billable should be input+output, got %d (%+v)", got.Billable(), got)
	}

	// A failed call is an invocation with no tokens: the provider refused, so
	// what it consumed is unknown and must not be invented.
	if err := s.RecordUsage(UsageRecord{
		UserID: "u_1", Agent: "bot", Model: "m", Kind: "chat",
		Input: 999, Output: 999, OK: false, At: at,
	}, false); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	got, _ = s.UsageForDay("u_1", day)
	if got.Input != 200 || got.Output != 40 {
		t.Fatalf("a failed call must not bill tokens: %+v", got)
	}
	if got.Calls != 3 || got.Failed != 1 {
		t.Fatalf("failed call must count as an attempt: %+v", got)
	}

	// A different model is a different slice.
	if err := s.RecordUsage(UsageRecord{
		UserID: "u_1", Agent: "bot", Model: "other", Kind: "chat",
		Input: 10, Output: 1, OK: true, At: at,
	}, false); err != nil {
		t.Fatalf("record other model: %v", err)
	}
	rows, err := s.UsageDaily(day)
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected two slices, got %+v", rows)
	}
	// Largest first: the m row (240) before other (11).
	if rows[0].Model != "m" || rows[1].Model != "other" {
		t.Fatalf("daily ordering: %+v", rows)
	}
}

// The day bucket is UTC, so two calls either side of local midnight land
// together when they belong together.
func TestUsageDayBucketing(t *testing.T) {
	s := testStore(t)
	late := time.Date(2026, 9, 21, 23, 59, 0, 0, time.UTC)
	early := time.Date(2026, 9, 22, 0, 1, 0, 0, time.UTC)
	mustRecord(t, s, "u_1", 7, late)
	mustRecord(t, s, "u_1", 3, early)

	d1, _ := s.UsageForDay("u_1", "2026-09-21")
	d2, _ := s.UsageForDay("u_1", "2026-09-22")
	if d1.Input != 7 || d2.Input != 3 {
		t.Fatalf("day buckets wrong: d1=%+v d2=%+v", d1, d2)
	}
	// An unknown user or day is zero, not an error.
	zero, err := s.UsageForDay("u_nobody", "2026-09-21")
	if err != nil || zero.Calls != 0 {
		t.Fatalf("unknown user should be zero: %+v err=%v", zero, err)
	}
}

// The event log is opt-in per write: the rollup answers quota questions, the
// detail rows are for drilling in.
func TestUsageEventsAreOptIn(t *testing.T) {
	s := testStore(t)
	at := time.Now()
	if err := s.RecordUsage(UsageRecord{
		UserID: "u_1", Agent: "bot", Model: "m", Kind: "chat",
		Input: 5, Output: 1, OK: true, At: at,
	}, false); err != nil {
		t.Fatalf("record: %v", err)
	}
	evs, err := s.UsageEvents("u_1", at.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("no event rows expected when the detail log is off: %+v", evs)
	}

	if err := s.RecordUsage(UsageRecord{
		UserID: "u_1", Agent: "bot", Model: "m", Kind: "stream",
		Input: 5, Output: 1, Cached: 3, OK: true, At: at,
	}, true); err != nil {
		t.Fatalf("record with event: %v", err)
	}
	evs, err = s.UsageEvents("u_1", at.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != "stream" || evs[0].Cached != 3 || !evs[0].OK {
		t.Fatalf("event row wrong: %+v", evs)
	}
	// Another user's events stay out of scope.
	if evs, _ := s.UsageEvents("u_2", at.Add(-time.Hour), 10); len(evs) != 0 {
		t.Fatalf("events must be per user: %+v", evs)
	}
}

func TestPruneUsage(t *testing.T) {
	s := testStore(t)
	old := time.Now().AddDate(0, 0, -60)
	mustRecord(t, s, "u_1", 4, old)
	mustRecord(t, s, "u_1", 6, time.Now())

	if err := s.PruneUsage(time.Now().AddDate(0, 0, -30)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got, _ := s.UsageForDay("u_1", DayKey(old)); got.Calls != 0 {
		t.Fatalf("old rollup should be pruned: %+v", got)
	}
	if got, _ := s.UsageForDay("u_1", DayKey(time.Now())); got.Input != 6 {
		t.Fatalf("recent rollup must survive: %+v", got)
	}
}

func mustRecord(t *testing.T, s *Store, user string, input int, at time.Time) {
	t.Helper()
	if err := s.RecordUsage(UsageRecord{
		UserID: user, Agent: "bot", Model: "m", Kind: "chat",
		Input: input, Output: 1, OK: true, At: at,
	}, false); err != nil {
		t.Fatalf("record: %v", err)
	}
}
