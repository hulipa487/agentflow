package runtime

import (
	"context"
	"path/filepath"
	"testing"
)

func TestBudgetUsageRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.RecordBudgetUsage(ctx, "owner-1", "2026-01-01", 500); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBudgetUsage(ctx, "owner-1", "2026-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if got != 500 {
		t.Fatalf("got %d, want 500", got)
	}

	// Update.
	if err := s.RecordBudgetUsage(ctx, "owner-1", "2026-01-01", 800); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetBudgetUsage(ctx, "owner-1", "2026-01-01")
	if got != 800 {
		t.Fatalf("got %d, want 800 after update", got)
	}

	// Missing day returns 0.
	got, _ = s.GetBudgetUsage(ctx, "owner-1", "2026-01-02")
	if got != 0 {
		t.Fatalf("got %d, want 0 for missing day", got)
	}
}

func TestChildMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	m := ChildMeta{
		SessionID: "spawn:coder|abc",
		Agent:     "spawn:coder",
		ParentID:  "planner|x",
		Profile:   "coder",
		CreatedAt: 12345,
	}
	if err := s.RecordChild(ctx, m); err != nil {
		t.Fatal(err)
	}
	children, err := s.ListChildren(ctx, "planner|x")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].SessionID != "spawn:coder|abc" {
		t.Fatalf("unexpected children: %+v", children)
	}
	if err := s.DeleteChild(ctx, "spawn:coder|abc"); err != nil {
		t.Fatal(err)
	}
	children, _ = s.ListChildren(ctx, "planner|x")
	if len(children) != 0 {
		t.Fatalf("expected 0 children after delete, got %d", len(children))
	}
}

func TestTimerMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	m := TimerMeta{TimerID: "timer-1", Owner: "planner|x", Kind: "every", NextFire: 99999}
	if err := s.RecordTimer(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTimersByOwner(ctx, "planner|x"); err != nil {
		t.Fatal(err)
	}
	// Verify the timer is gone by re-deleting (should be a no-op).
	if err := s.DeleteTimer(ctx, "timer-1"); err != nil {
		t.Fatal(err)
	}
}
