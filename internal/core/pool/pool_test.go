package pool

import (
	"sync/atomic"
	"testing"
	"time"
)

// The pool had no tests. It is small and it is load-bearing: every blocking op a
// loop issues runs here, so a job that never runs or a result that never arrives
// is a session that hangs rather than an error anyone sees.

func TestCallReturnsTheResult(t *testing.T) {
	p := New(2)
	got := <-Call(p, func() int { return 42 })
	if got != 42 {
		t.Fatalf("Call returned %d; want 42", got)
	}
}

func TestSubmitRunsTheJob(t *testing.T) {
	p := New(1)
	done := make(chan struct{})
	p.Submit(func() { close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a submitted job never ran")
	}
}

// TestZeroWorkersStillRuns: New clamps to one worker, so a pool built from a
// zero setting still makes progress. A pool with no workers would accept jobs
// and silently never run them.
func TestZeroWorkersStillRuns(t *testing.T) {
	for _, n := range []int{0, -3} {
		p := New(n)
		done := make(chan struct{})
		p.Submit(func() { close(done) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("New(%d) produced a pool that never runs its jobs", n)
		}
	}
}

// TestConcurrentCallsAllComplete: results are per-call, so interleaving must not
// cross them.
func TestConcurrentCallsAllComplete(t *testing.T) {
	p := New(4)
	results := make([]<-chan int, 200)
	for i := range results {
		results[i] = Call(p, func() int { return i })
	}
	sum := 0
	for _, c := range results {
		select {
		case v := <-c:
			sum += v
		case <-time.After(10 * time.Second):
			t.Fatal("a Call result never arrived")
		}
	}
	if want := 199 * 200 / 2; sum != want {
		t.Fatalf("sum = %d; want %d — a result was lost or crossed with another", sum, want)
	}
}

// TestBacklogAppliesBackpressure: Submit blocks once the queue is full and every
// worker is busy. That is the documented backpressure point — the caller learns
// it is falling behind instead of the queue growing without limit — so a
// regression to an unbounded queue should fail here rather than in production.
func TestBacklogAppliesBackpressure(t *testing.T) {
	p := New(1)
	release := make(chan struct{})
	started := make(chan struct{})
	p.Submit(func() { close(started); <-release })
	<-started // the only worker is now occupied

	// Comfortably more than any plausible buffer depth.
	var submitted atomic.Int64
	drained := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			p.Submit(func() {})
			submitted.Add(1)
		}
		close(drained)
	}()

	select {
	case <-drained:
		t.Fatalf("all 1000 jobs were accepted while the only worker was busy; the backlog is unbounded")
	case <-time.After(200 * time.Millisecond):
		// Expected: parked on the full queue.
	}

	close(release)
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the backlog never drained after the worker was released")
	}
}
