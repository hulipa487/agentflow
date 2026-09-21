package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"
)

// postgresDSN points these cases at a server:
//
//	AGENTFLOW_TEST_POSTGRES='postgres://…' go test ./internal/core/lease/
//
// Unset, they skip: SQLite is the default everywhere. Lease names are tagged
// per run, so the suite is re-runnable against a shared server.
var postgresDSN = os.Getenv("AGENTFLOW_TEST_POSTGRES")

func pgName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "lease-test:" + hex.EncodeToString(b)
}

// The compare-and-set that decides a fleet's singleton work, on the backend a
// fleet runs on: one holder at a time, taken over once it expires, handed over
// on release, and exactly one winner among contenders.
func TestPostgresLeaseArbitration(t *testing.T) {
	if postgresDSN == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
	}
	ctx := context.Background()
	a := openAt(t, postgresDSN, "instance-a")
	b := openAt(t, postgresDSN, "instance-b")
	name := pgName(t)

	if ok, err := a.Acquire(ctx, name); err != nil || !ok {
		t.Fatalf("a should take a free lease: ok=%v err=%v", ok, err)
	}
	if ok, err := b.Acquire(ctx, name); err != nil || ok {
		t.Fatalf("b must not take a live lease: ok=%v err=%v", ok, err)
	}
	// The holder renews rather than losing its own lease.
	if ok, err := a.Acquire(ctx, name); err != nil || !ok {
		t.Fatalf("the holder must be able to renew: ok=%v err=%v", ok, err)
	}
	// An expired lease is taken over, and then it is one-way.
	expire(t, a, name)
	if ok, err := b.Acquire(ctx, name); err != nil || !ok {
		t.Fatalf("b should take an expired lease: ok=%v err=%v", ok, err)
	}
	if ok, err := a.Acquire(ctx, name); err != nil || ok {
		t.Fatalf("a must not take a lease b now holds: ok=%v err=%v", ok, err)
	}
	// A non-holder's release does nothing; the holder's hands it over at once.
	if err := a.Release(ctx, name); err != nil {
		t.Fatalf("release by a non-holder: %v", err)
	}
	if ok, _ := a.Acquire(ctx, name); ok {
		t.Fatal("a non-holder's release must not free the lease")
	}
	if err := b.Release(ctx, name); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok, err := a.Acquire(ctx, name); err != nil || !ok {
		t.Fatalf("a should take a released lease: ok=%v err=%v", ok, err)
	}
}

// A claim's own deadline is honoured on the server too: an interval trigger
// depends on it outliving the next fire.
func TestPostgresAcquireForHonoursTheDeadline(t *testing.T) {
	if postgresDSN == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
	}
	ctx := context.Background()
	a := openAt(t, postgresDSN, "instance-a")
	b := openAt(t, postgresDSN, "instance-b")
	name := pgName(t)

	if ok, err := a.AcquireFor(ctx, name, 50*time.Millisecond); err != nil || !ok {
		t.Fatalf("acquire for a short deadline: ok=%v err=%v", ok, err)
	}
	time.Sleep(150 * time.Millisecond)
	if ok, err := b.AcquireFor(ctx, name, time.Minute); err != nil || !ok {
		t.Fatalf("a claim past its deadline must be claimable: ok=%v err=%v", ok, err)
	}
}

// Contenders racing for one lease produce one winner, on the server as in a
// file: without it, two instances would fire the same trigger.
func TestPostgresConcurrentContendersProduceOneWinner(t *testing.T) {
	if postgresDSN == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
	}
	const contenders = 6
	name := pgName(t)
	ms := make([]*Manager, contenders)
	for i := range ms {
		ms[i] = openAt(t, postgresDSN, "instance-"+string(rune('a'+i)))
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		start = make(chan struct{})
	)
	for _, m := range ms {
		wg.Add(1)
		go func(m *Manager) {
			defer wg.Done()
			<-start
			ok, err := m.Acquire(context.Background(), name)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if ok {
				wins++
			}
		}(m)
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("exactly one contender must win, got %d", wins)
	}
}
