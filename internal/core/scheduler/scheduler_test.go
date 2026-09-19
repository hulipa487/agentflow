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
	id, err := s.After("owner-1", 50*time.Millisecond, func(owner string, id TimerID) {
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
	id, err := s.Every("owner-1", 1*time.Second/4, func(owner string, id TimerID) {
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
	_, _ = s.Every("owner-a", 1*time.Second/2, func(owner string, id TimerID) { fires.Add(1) })
	_, _ = s.Every("owner-b", 1*time.Second/2, func(owner string, id TimerID) { fires.Add(1) })
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

// TestFiresCarryTimerID: two timers on one owner deliver distinguishable
// fires — each carrying the registering timer's id — so a session can run
// multiple independent timers.
func TestFiresCarryTimerID(t *testing.T) {
	s := New(nil)
	var mu sync.Mutex
	got := map[TimerID]int{}
	capture := func(owner string, id TimerID) {
		mu.Lock()
		got[id]++
		mu.Unlock()
	}
	id1, err := s.Every("owner-1", 100*time.Millisecond, capture)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.Every("owner-1", 130*time.Millisecond, capture)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Cancel(id1)
	defer s.Cancel(id2)
	if id1 == id2 {
		t.Fatal("two timers got the same id")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n1, n2 := got[id1], got[id2]
		mu.Unlock()
		if n1 > 0 && n2 > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("fires not attributable: id1=%d id2=%d", got[id1], got[id2])
}
