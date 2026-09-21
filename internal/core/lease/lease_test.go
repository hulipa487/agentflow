package lease

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Two managers over one store are two instances of the engine sharing a
// database — the smallest honest model of a fleet, and one that runs anywhere.
func twoManagers(t *testing.T) (a, b *Manager) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lease.db")
	a = openAt(t, path, "instance-a")
	b = openAt(t, path, "instance-b")
	return a, b
}

func openAt(t *testing.T, path, owner string) *Manager {
	t.Helper()
	m, err := Open(path, owner, time.Minute, nil)
	if err != nil {
		t.Fatalf("open %s: %v", owner, err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// expire backdates a lease, standing in for a holder that stopped renewing —
// the only way a takeover becomes possible.
func expire(t *testing.T, m *Manager, name string) {
	t.Helper()
	if _, err := m.st.Exec(context.Background(),
		`UPDATE leases SET expires_at = ? WHERE name = ?`, time.Now().Add(-time.Hour).UnixNano(), name); err != nil {
		t.Fatalf("expire: %v", err)
	}
}

// One holder at a time: while a lease is live, every other instance is told no.
func TestOneHolderAtATime(t *testing.T) {
	a, b := twoManagers(t)
	ctx := context.Background()

	ok, err := a.Acquire(ctx, "trigger:digest")
	if err != nil || !ok {
		t.Fatalf("a should take a free lease: ok=%v err=%v", ok, err)
	}
	if ok, err := b.Acquire(ctx, "trigger:digest"); err != nil || ok {
		t.Fatalf("b must not take a live lease: ok=%v err=%v", ok, err)
	}
	// The holder renews rather than losing its own lease.
	if ok, err := a.Acquire(ctx, "trigger:digest"); err != nil || !ok {
		t.Fatalf("the holder must be able to renew: ok=%v err=%v", ok, err)
	}
	// A different name is a different lease.
	if ok, err := b.Acquire(ctx, "trigger:other"); err != nil || !ok {
		t.Fatalf("an unrelated lease is free: ok=%v err=%v", ok, err)
	}
}

// A lease outlives nothing: once it expires, another instance takes over.
// This is how a fleet recovers from an instance that died mid-job.
func TestExpiredLeaseIsTakenOver(t *testing.T) {
	a, b := twoManagers(t)
	ctx := context.Background()

	if ok, err := a.Acquire(ctx, "files-gc"); err != nil || !ok {
		t.Fatalf("a should take it: ok=%v err=%v", ok, err)
	}
	expire(t, a, "files-gc")
	if ok, err := b.Acquire(ctx, "files-gc"); err != nil || !ok {
		t.Fatalf("b should take an expired lease: ok=%v err=%v", ok, err)
	}
	// And now a is the one told no — the takeover is one-way until b expires.
	if ok, err := a.Acquire(ctx, "files-gc"); err != nil || ok {
		t.Fatalf("a must not take a lease b now holds: ok=%v err=%v", ok, err)
	}
}

// Releasing hands the work over at once instead of making the fleet wait out
// the expiry — and only the holder can release.
func TestReleaseHandsOverImmediately(t *testing.T) {
	a, b := twoManagers(t)
	ctx := context.Background()

	if ok, _ := a.Acquire(ctx, "journal-prune"); !ok {
		t.Fatal("a should take it")
	}
	// A non-holder's release must not evict the live owner.
	if err := b.Release(ctx, "journal-prune"); err != nil {
		t.Fatalf("release by a non-holder: %v", err)
	}
	if ok, _ := b.Acquire(ctx, "journal-prune"); ok {
		t.Fatal("a non-holder's release must not free the lease")
	}
	if err := a.Release(ctx, "journal-prune"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok, err := b.Acquire(ctx, "journal-prune"); err != nil || !ok {
		t.Fatalf("b should take a released lease: ok=%v err=%v", ok, err)
	}
}

// The property the whole design rests on: contenders racing for the same lease
// produce exactly one winner, however many there are and whatever the timing.
// Without it, two instances would fire the same trigger.
func TestConcurrentContendersProduceOneWinner(t *testing.T) {
	const contenders = 8
	path := filepath.Join(t.TempDir(), "lease.db")
	ms := make([]*Manager, contenders)
	for i := range ms {
		ms[i] = openAt(t, path, "instance-"+string(rune('a'+i)))
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		wins   int
		losses int
		start  = make(chan struct{})
	)
	for _, m := range ms {
		wg.Add(1)
		go func(m *Manager) {
			defer wg.Done()
			<-start
			ok, err := m.Acquire(context.Background(), "trigger:race")
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if ok {
				wins++
			} else {
				losses++
			}
		}(m)
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("exactly one contender must win, got %d wins and %d losses", wins, losses)
	}
	if losses != contenders-1 {
		t.Fatalf("every other contender must be told no, got %d losses", losses)
	}
}

// An owner string has to be unique per process: two instances that believe they
// are one would renew each other's leases and both run the singleton work.
func TestOwnerIDIsUniquePerProcess(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := OwnerID()
		if id == "" {
			t.Fatal("empty owner id")
		}
		if seen[id] {
			t.Fatalf("duplicate owner id %q", id)
		}
		seen[id] = true
	}
}

// An empty owner is refused rather than defaulted: a manager that cannot say
// who it is cannot hold a lease on anyone's behalf.
func TestOpenRejectsEmptyOwner(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "lease.db"), "", time.Minute, nil); err == nil {
		t.Fatal("an empty owner must not open a lease manager")
	}
}
