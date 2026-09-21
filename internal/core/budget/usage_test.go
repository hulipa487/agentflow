package budget

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ledgerSource is a stand-in for the token ledger: a figure that a test sets,
// and a count of how often the pool asked.
type ledgerSource struct {
	total int64
	err   error
	calls int
}

func (l *ledgerSource) fn() func(context.Context) (int64, error) {
	return func(context.Context) (int64, error) {
		l.calls++
		if l.err != nil {
			return 0, l.err
		}
		return l.total, nil
	}
}

// A daily budget belongs to the agent, not to the process enforcing it. A pool
// that starts at zero because *this* instance has not spent anything would hand
// every instance the full budget.
func TestUsageSourceIsTheBaseline(t *testing.T) {
	led := &ledgerSource{total: 900}
	p := NewPool(1000)
	p.SetUsageSource(led.fn())
	ctx := context.Background()

	p.Refresh(ctx)
	if p.Used() != 900 {
		t.Fatalf("the pool should adopt the shared figure, got %d", p.Used())
	}
	if _, err := p.Reserve(200); !errors.Is(err, ErrExhausted) {
		t.Fatalf("900 of 1000 spent leaves no room for 200: %v", err)
	}
	if _, err := p.Reserve(50); err != nil {
		t.Fatalf("50 should fit: %v", err)
	}
}

// A restart must not forgive the day. The pool is new; the ledger is not.
func TestRestartDoesNotForgiveSpend(t *testing.T) {
	led := &ledgerSource{total: 1000}
	p := NewPool(1000) // as if the process had just started
	p.SetUsageSource(led.fn())
	if _, err := p.Reserve(1); !errors.Is(err, ErrExhausted) {
		t.Fatalf("an exhausted agent must stay exhausted across a restart: %v", err)
	}
}

// Between refreshes the pool counts what this process commits, and the next
// refresh replaces that count with the ledger rather than adding to it —
// otherwise the instance's own calls would be counted twice.
func TestLocalCommitsAreReplacedByTheLedger(t *testing.T) {
	led := &ledgerSource{total: 0}
	p := NewPool(1000)
	p.SetUsageSource(led.fn())
	ctx := context.Background()

	lease, err := p.Reserve(100)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := p.Commit(lease, 100); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if p.Used() != 100 {
		t.Fatalf("a local commit should count immediately, got %d", p.Used())
	}
	// The ledger has since recorded it, plus another instance's 500.
	led.total = 600
	p.SetRefreshInterval(time.Nanosecond)
	// The clock, not the interval, is the limit here: Windows timers are
	// coarser than a nanosecond, so give the staleness check something to see.
	time.Sleep(5 * time.Millisecond)
	p.Refresh(ctx)
	if got := p.Used(); got != 600 {
		t.Fatalf("the ledger is the whole truth, got %d want 600", got)
	}
}

// The read is throttled: a reservation must not turn into a query per call.
func TestRefreshIsThrottled(t *testing.T) {
	led := &ledgerSource{total: 10}
	p := NewPool(1000)
	p.SetUsageSource(led.fn())
	p.SetRefreshInterval(time.Hour)
	ctx := context.Background()

	p.Refresh(ctx)
	p.Refresh(ctx)
	p.Refresh(ctx)
	if led.calls != 1 {
		t.Fatalf("a stale check should read once, read %d times", led.calls)
	}
}

// A ledger that cannot be read leaves the pool counting what it knows: fail
// open, like the per-user quota, because accounting must not stop the runtime.
func TestUnreadableLedgerLeavesThePoolCountingLocally(t *testing.T) {
	led := &ledgerSource{err: errors.New("ledger unreachable")}
	p := NewPool(1000)
	p.SetUsageSource(led.fn())
	ctx := context.Background()

	p.Refresh(ctx)
	if p.Used() != 0 {
		t.Fatalf("a failed read must not change the count, got %d", p.Used())
	}
	if _, err := p.Reserve(100); err != nil {
		t.Fatalf("an unreadable ledger must not refuse calls: %v", err)
	}
}

// The day rolls over on the pool's clock, and the baseline has to roll with it:
// a stale figure from yesterday would keep the agent exhausted into today.
func TestResetDailyClearsTheBaseline(t *testing.T) {
	led := &ledgerSource{total: 1000}
	p := NewPool(1000)
	p.SetUsageSource(led.fn())
	p.Refresh(context.Background())
	if _, err := p.Reserve(1); !errors.Is(err, ErrExhausted) {
		t.Fatalf("expected the pool to be exhausted: %v", err)
	}
	led.total = 0 // a new day
	p.ResetDaily()
	if _, err := p.Reserve(500); err != nil {
		t.Fatalf("a new day starts empty: %v", err)
	}
}

// A rolling window is this process's commits over the trailing period, which
// the ledger's day-granular rows cannot express. A source installed on one is
// inert rather than misleading.
func TestWindowedPoolIgnoresTheSource(t *testing.T) {
	led := &ledgerSource{total: 5000}
	p := NewPool(1000)
	p.SetWindow(time.Hour)
	p.SetUsageSource(led.fn())
	ctx := context.Background()

	p.Refresh(ctx)
	if p.Used() != 0 {
		t.Fatalf("a windowed pool must not adopt a daily figure, got %d", p.Used())
	}
	if led.calls != 0 {
		t.Fatalf("a windowed pool should not read the ledger at all, read %d times", led.calls)
	}
	if _, err := p.Reserve(500); err != nil {
		t.Fatalf("reserve: %v", err)
	}
}
