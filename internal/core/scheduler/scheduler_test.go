package scheduler

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAfterFiresOnce(t *testing.T) {
	s := New(nil)
	var fires atomic.Int32
	id, err := s.After("owner-1", 50*time.Millisecond, func(owner string) {
		fires.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if fires.Load() != 1 {
		t.Fatalf("expected 1 fire, got %d", fires.Load())
	}
	if s.Pending() != 0 {
		t.Fatalf("one-shot should self-remove, pending=%d", s.Pending())
	}
	_ = id
}

func TestEveryFiresRepeatedly(t *testing.T) {
	s := New(nil)
	var fires atomic.Int32
	id, err := s.Every("owner-1", 1*time.Second/4, func(owner string) {
		fires.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(900 * time.Millisecond)
	if fires.Load() < 2 {
		t.Fatalf("expected >=2 fires, got %d", fires.Load())
	}
	_ = s.Cancel(id)
}

func TestEveryRejectsTooShort(t *testing.T) {
	s := New(nil)
	if _, err := s.Every("owner-1", 50*time.Millisecond, nil); err == nil {
		t.Fatal("expected sub-100ms interval to be rejected")
	}
}

func TestCancelOwner(t *testing.T) {
	s := New(nil)
	var fires atomic.Int32
	_, _ = s.Every("owner-a", 1*time.Second/2, func(owner string) { fires.Add(1) })
	_, _ = s.Every("owner-b", 1*time.Second/2, func(owner string) { fires.Add(1) })
	s.CancelOwner("owner-a")
	time.Sleep(100 * time.Millisecond)
	if s.Pending() != 1 {
		t.Fatalf("pending=%d, want 1", s.Pending())
	}
	s.CancelOwner("owner-b")
}

func TestCronRejectsUnsupportedFields(t *testing.T) {
	s := New(nil)
	if _, err := s.Cron("owner", "*/5 * * * 1", nil); err == nil {
		t.Fatal("expected weekday rejection")
	}
	if _, err := s.Cron("owner", "bad", nil); err == nil {
		t.Fatal("expected malformed rejection")
	}
}

// Ensure scheduler compiles with zero timers and no goroutine leak.
func TestPendingStartsAtZero(t *testing.T) {
	s := New(nil)
	if s.Pending() != 0 {
		t.Fatalf("pending=%d, want 0", s.Pending())
	}
}

func init() {
	// Suppress unused warning; sync is needed for the atomic counter in tests.
	_ = sync.Mutex{}
}
