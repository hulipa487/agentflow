package accounting

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/core/runtime"
	"github.com/google/uuid"
)

// usageOnly is a ledger with a fixed spend: the holds tests care about
// reservations, not about writing usage.
type usageOnly struct {
	runtime.Ledger
	used int64
}

func (u usageOnly) UsageForDay(string, string) (runtime.UsageTotals, error) {
	return runtime.UsageTotals{Input: u.used}, nil
}

// TestSharedQuotaHoldsSQLite exercises the shared reservation table end to end
// on the backend that always runs: reserve, refuse the over-limit reservation,
// report the in-flight total, release, admit again.
func TestSharedQuotaHoldsSQLite(t *testing.T) {
	q, err := NewShared(filepath.Join(t.TempDir(), "agentflow.db"), usageOnly{used: 100}, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	ctx := context.Background()

	first, err := q.Reserve(ctx, "u1", 600)
	if err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if first == nil {
		t.Fatal("first reservation returned no lease")
	}
	if _, err := q.Reserve(ctx, "u1", 400); err == nil {
		t.Fatal("a reservation past the limit was admitted")
	} else if !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("refusal = %v, want ErrQuotaExhausted", err)
	}

	_, limit, inFlight, err := q.Status("u1")
	if err != nil {
		t.Fatal(err)
	}
	if limit != 1000 || inFlight != 600 {
		t.Fatalf("status = limit %d in-flight %d, want 1000/600", limit, inFlight)
	}

	first.Release()
	if _, err := q.Reserve(ctx, "u1", 400); err != nil {
		t.Fatalf("reservation after release: %v", err)
	}
}

// TestSharedQuotaHoldsPostgresConcurrent is the race the shared holds exist
// for: many instances reserve for one user at once, and the total admitted may
// never exceed the limit. Gated on AGENTFLOW_TEST_POSTGRES like the other
// backend integration tests.
func TestSharedQuotaHoldsPostgresConcurrent(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES not set")
	}
	limit := int64(1000)
	amount := int64(100)
	want := int(limit / amount)

	// Two quota instances stand in for two engine processes sharing the store.
	// They share this process's connection pool, but the transaction and the
	// advisory lock are the real mechanism.
	q1, err := NewShared(dsn, usageOnly{}, nil, limit)
	if err != nil {
		t.Fatal(err)
	}
	defer q1.Close()
	q2, err := NewShared(dsn, usageOnly{}, nil, limit)
	if err != nil {
		t.Fatal(err)
	}
	defer q2.Close()

	user := "quota-test-" + uuid.NewString()
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		q := q1
		if i%2 == 1 {
			q = q2
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := q.Reserve(context.Background(), user, amount)
			if err == nil && lease != nil {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := int(admitted.Load()); got != want {
		t.Fatalf("admitted %d reservations of %d, want exactly %d (limit %d)", got, amount, want, limit)
	}

	// And the in-flight total the instances see is the admitted amount.
	_, _, inFlight, err := q2.Status(user)
	if err != nil {
		t.Fatal(err)
	}
	if inFlight != int64(want)*amount {
		t.Fatalf("in-flight = %d, want %d", inFlight, int64(want)*amount)
	}

	// Expired holds are swept and stop counting; a live one does not.
	if _, err := q1.SweepHolds(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, _, inFlight, err = q2.Status(user)
	if err != nil {
		t.Fatal(err)
	}
	if inFlight != 0 {
		t.Fatalf("after sweeping expired holds in-flight = %d, want 0", inFlight)
	}
}

// A call that outlasts the hold TTL must stay bounded: the lease renews its hold
// while it is open, and stops when it ends.
func TestSharedQuotaHoldOutlivesItsTTLWhileOpen(t *testing.T) {
	oldTTL := holdTTL
	holdTTL = 150 * time.Millisecond
	defer func() { holdTTL = oldTTL }()
	q, err := NewShared(filepath.Join(t.TempDir(), "agentflow.db"), usageOnly{used: 0}, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	ctx := context.Background()
	l, err := q.Reserve(ctx, "u1", 800)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * holdTTL)
	if _, _, inFlight, err := q.Status("u1"); err != nil || inFlight != 800 {
		t.Fatalf("in flight after 3 TTLs = %d (%v), want the open hold's 800", inFlight, err)
	}
	if _, err := q.Reserve(ctx, "u1", 400); err == nil {
		t.Fatal("an open hold lapsed and let an over-limit call in")
	}
	l.Release()
	again, err := q.Reserve(ctx, "u1", 400)
	if err != nil {
		t.Fatalf("reservation after release: %v", err)
	}
	again.Release()
}
