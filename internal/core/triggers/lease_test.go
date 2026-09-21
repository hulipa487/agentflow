package triggers

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/lease"
	"agentflow/internal/core/session"
)

// countingService builds a Service whose deliveries are counted, optionally
// arbitrated by a lease. Two of these over one lease store are two instances of
// the engine with the same trigger declaration — the fleet case.
func countingService(t *testing.T, leasePath, owner string) (*Service, *int32) {
	t.Helper()
	var n int32
	s := New(
		func(string) (string, bool) { return "worker", true },
		func(_, _ string, _ session.Message) error {
			atomic.AddInt32(&n, 1)
			return nil
		},
		0, nil,
	)
	if leasePath != "" {
		m, err := lease.Open(leasePath, owner, time.Second, nil)
		if err != nil {
			t.Fatalf("lease open: %v", err)
		}
		t.Cleanup(func() { _ = m.Close() })
		s.SetLeases(m)
	}
	return s, &n
}

// Every instance runs the trigger's timer — they have to, because the owner can
// change — and the lease is what makes an occurrence produce one message
// instead of one per instance.
func TestOnlyTheLeaseHolderFiresEachOccurrence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease.db")
	a, countA := countingService(t, path, "instance-a")
	b, countB := countingService(t, path, "instance-b")
	j := &job{trigger: config.Trigger{Name: "digest"}, kind: "every"}

	a.fire(j)
	if got := atomic.LoadInt32(countA); got != 1 {
		t.Fatalf("the first instance to fire should deliver: %d", got)
	}
	// The other instance's timer reaches the same occurrence and delivers
	// nothing: this is the duplicate the lease exists to prevent.
	b.fire(j)
	if got := atomic.LoadInt32(countB); got != 0 {
		t.Fatalf("a non-holder must not fire: %d deliveries", got)
	}
	// The holder keeps the trigger without waiting out the expiry.
	a.fire(j)
	if got := atomic.LoadInt32(countA); got != 2 {
		t.Fatalf("the holder should keep firing: %d", got)
	}

	// A restart — a deploy, a move to another host — hands the trigger over at
	// once rather than making the fleet wait out the lease, and then it is the
	// other instance that fires.
	a.Stop()
	b.fire(j)
	if got := atomic.LoadInt32(countB); got != 1 {
		t.Fatalf("a stopped instance must hand its triggers over: %d deliveries", got)
	}
	a.fire(j)
	if got := atomic.LoadInt32(countA); got != 2 {
		t.Fatalf("the previous holder must not fire after handing over: %d", got)
	}
}

// A claim has to outlast the next occurrence, or it lapses in between and the
// next instance whose timer happens to come up takes the trigger — which for an
// `every:` schedule with a phase offset means the trigger fires more often than
// the deployment declared.
func TestLeaseTTLCoversTheNextOccurrence(t *testing.T) {
	now := time.Now()

	for _, interval := range []time.Duration{time.Second, 5 * time.Minute, time.Hour} {
		j := &job{kind: "every", every: interval}
		if got := j.leaseTTL(now); got < interval {
			t.Errorf("every %s: a claim of %s would lapse before the next occurrence", interval, got)
		}
	}
	// Even a fast trigger claims more than its own interval: the claim is what
	// keeps the other instances' timers quiet.
	if got := (&job{kind: "every", every: 100 * time.Millisecond}).leaseTTL(now); got < minLeaseTTL {
		t.Errorf("a sub-second interval should claim at least the minimum, got %s", got)
	}

	// A cron schedule is absolute, so its claim may end *before* the next
	// occurrence: every instance's timer comes up at the same instant, the
	// compare-and-set picks one, and a holder that died costs one occurrence
	// rather than a whole period.
	daily, err := ParseCron("0 3 * * *", time.UTC)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	before := time.Date(2026, 9, 21, 2, 0, 0, 0, time.UTC) // an hour ahead of the next fire
	if got := (&job{kind: "cron", sched: daily}).leaseTTL(before); got >= time.Hour {
		t.Errorf("a daily claim should end before the next occurrence, got %s", got)
	}

	// A schedule that fires every minute claims the minimum, not a negative.
	minute, err := ParseCron("* * * * *", time.UTC)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	justBefore := time.Date(2026, 9, 21, 2, 0, 50, 0, time.UTC)
	if got := (&job{kind: "cron", sched: minute}).leaseTTL(justBefore); got != minLeaseTTL {
		t.Errorf("a dense schedule should claim the minimum, got %s", got)
	}
}

// Without a lease manager nothing arbitrates, and every trigger fires here —
// which is what a deployment that runs alone did before there was a lease, and
// must keep doing.
func TestWithoutLeasesEveryFireDelivers(t *testing.T) {
	s, count := countingService(t, "", "")
	j := &job{trigger: config.Trigger{Name: "digest"}, kind: "every"}
	s.fire(j)
	s.fire(j)
	if got := atomic.LoadInt32(count); got != 2 {
		t.Fatalf("with no lease installed every fire should deliver, got %d", got)
	}
}

// A store that cannot say who owns the trigger cannot arbitrate it, and firing
// on every instance would deliver the same message once per instance. So a
// lease that cannot be taken refuses the fire: the next interval repeats the
// work anyway, and a duplicate cannot be taken back.
func TestLeaseFailureRefusesToFire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease.db")
	s, count := countingService(t, path, "instance-a")
	// Close the lease manager out from under the service: its store is gone,
	// which is what an unreachable database looks like from here.
	_ = s.leases.Close()

	s.fire(&job{trigger: config.Trigger{Name: "digest"}, kind: "every"})
	if got := atomic.LoadInt32(count); got != 0 {
		t.Fatalf("a trigger must not fire when the lease cannot be taken, got %d deliveries", got)
	}
}

// A trigger's lease is per trigger: two different triggers on two instances do
// not block each other, so a fleet spreads the work rather than pinning all of
// it to whichever instance booted first.
func TestDifferentTriggersDoNotContend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease.db")
	a, countA := countingService(t, path, "instance-a")
	b, countB := countingService(t, path, "instance-b")

	a.fire(&job{trigger: config.Trigger{Name: "digest"}, kind: "every"})
	b.fire(&job{trigger: config.Trigger{Name: "report"}, kind: "every"})

	if got := atomic.LoadInt32(countA); got != 1 {
		t.Fatalf("instance a should fire its own trigger: %d", got)
	}
	if got := atomic.LoadInt32(countB); got != 1 {
		t.Fatalf("instance b should fire a different trigger: %d", got)
	}
}
