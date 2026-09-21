package accounting

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agentflow/internal/core/identity"
	"agentflow/internal/core/runtime"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fixture builds a quota over a real ledger and a real profile store.
func fixture(t *testing.T, defaultPerDay, profileLimit int64) (*Quota, *identity.Registry, *runtime.Store, string) {
	t.Helper()
	dir := t.TempDir()
	ledger, err := runtime.Open(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	reg, err := identity.Open(filepath.Join(dir, "identity.db"), discard())
	if err != nil {
		t.Fatalf("open identity: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	p, err := reg.CreateProfile("Oscar", "")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if profileLimit > 0 {
		limit := profileLimit
		if err := reg.Update(p.UserID, nil, nil, &limit); err != nil {
			t.Fatalf("set profile limit: %v", err)
		}
	}
	return New(ledger, reg.LimitFor, defaultPerDay), reg, ledger, p.UserID
}

func spend(t *testing.T, ledger *runtime.Store, userID string, input int) {
	t.Helper()
	if err := ledger.RecordUsage(runtime.UsageRecord{
		UserID: userID, Agent: "bot", Model: "m", Kind: "chat",
		Input: input, Output: 0, OK: true, At: time.Now(),
	}, false); err != nil {
		t.Fatalf("record: %v", err)
	}
}

// A context with no user has no account to charge, and a zero limit means
// unlimited: neither should ever refuse a call.
func TestQuotaWithoutAUserOrALimit(t *testing.T) {
	q, _, _, userID := fixture(t, 0, 0)

	if lease, err := q.Reserve(context.Background(), "", 1_000_000); err != nil || lease != nil {
		t.Fatalf("a userless context must have no quota: lease=%v err=%v", lease, err)
	}
	if lease, err := q.Reserve(context.Background(), userID, 1_000_000); err != nil || lease != nil {
		t.Fatalf("a zero limit means unlimited: lease=%v err=%v", lease, err)
	}
	// A nil quota is safe to call (no quota configured).
	var nilQ *Quota
	if lease, err := nilQ.Reserve(context.Background(), userID, 10); err != nil || lease != nil {
		t.Fatalf("nil quota: lease=%v err=%v", lease, err)
	}
	if used, limit, _, err := nilQ.Status(userID); err != nil || used != 0 || limit != 0 {
		t.Fatalf("nil quota status: used=%d limit=%d err=%v", used, limit, err)
	}
}

// The limit accounts for tokens already spent today, read from the durable
// ledger — that is what makes a restart forgive nothing.
func TestQuotaCountsDurableSpend(t *testing.T) {
	q, _, ledger, userID := fixture(t, 100, 0)

	lease, err := q.Reserve(context.Background(), userID, 60)
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	lease.Release()

	// Spend 80 of the 100 in the ledger.
	spend(t, ledger, userID, 80)
	if _, err := q.Reserve(context.Background(), userID, 30); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("80 spent + 30 asked must exceed a 100 limit, got %v", err)
	}
	if _, err := q.Reserve(context.Background(), userID, 20); err != nil {
		t.Fatalf("20 should still fit: %v", err)
	}

	used, limit, _, err := q.Status(userID)
	if err != nil || used != 80 || limit != 100 {
		t.Fatalf("status: used=%d limit=%d err=%v", used, limit, err)
	}
}

// In-flight reservations are the concurrency guard: without them, N calls
// arriving together would each read the same spend and all be admitted.
func TestQuotaHoldsInFlightReservations(t *testing.T) {
	q, _, _, userID := fixture(t, 100, 0)

	first, err := q.Reserve(context.Background(), userID, 60)
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	// 60 is held but not yet spent; a second 60 has to see it.
	if _, err := q.Reserve(context.Background(), userID, 60); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("the in-flight hold must be counted, got %v", err)
	}
	if _, err := q.Reserve(context.Background(), userID, 40); err != nil {
		t.Fatalf("40 should fit beside the 60 held: %v", err)
	}

	// Releasing gives the room back.
	first.Release()
	first.Release() // idempotent: a double release must not corrupt the hold
	if _, err := q.Reserve(context.Background(), userID, 40); err != nil {
		t.Fatalf("after release the room should be back: %v", err)
	}
}

func TestQuotaConcurrentReservationsCannotOverspend(t *testing.T) {
	q, _, _, userID := fixture(t, 100, 0)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	// Every goroutine holds what it is granted, so the test measures
	// simultaneous holds — the property that stops concurrent calls from each
	// spending the last of a budget — rather than a sequence of them.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := q.Reserve(context.Background(), userID, 30); err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
				return
			} else if !errors.Is(err, ErrQuotaExhausted) {
				t.Errorf("unexpected reservation error: %v", err)
			}
		}()
	}
	wg.Wait()
	// 30 × 3 = 90 fits in 100; a fourth (120) cannot.
	if granted != 3 {
		t.Fatalf("expected exactly 3 simultaneous holds of 30 under a 100 limit, got %d", granted)
	}
}

// A profile's own limit overrides the deployment default, and a profile without
// one falls back to it.
func TestQuotaProfileOverride(t *testing.T) {
	q, reg, _, userID := fixture(t, 500, 120)

	if _, err := q.Reserve(context.Background(), userID, 200); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("the profile's 120 must beat the default 500, got %v", err)
	}
	if _, err := q.Reserve(context.Background(), userID, 100); err != nil {
		t.Fatalf("100 should fit under the profile limit: %v", err)
	}

	// Clearing the override falls back to the deployment default.
	zero := int64(0)
	if err := reg.Update(userID, nil, nil, &zero); err != nil {
		t.Fatalf("clear override: %v", err)
	}
	if _, err := q.Reserve(context.Background(), userID, 400); err != nil {
		t.Fatalf("with no override the default 500 applies: %v", err)
	}
}
