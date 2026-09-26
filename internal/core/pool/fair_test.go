package pool

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// The fairness policy: workers drain per-key queues round-robin, one job at a
// time per key, so one tenant's backlog cannot hold the pool. The old single
// FIFO could not tell whose op it was holding — that is the E18
// denial-of-service vector these tests pin shut.

// TestRoundRobinInterleavesTenants: with one worker and a deep queue for a,
// b's single job runs after exactly one of a's — not after all of them.
func TestRoundRobinInterleavesTenants(t *testing.T) {
	p := New(1, WithLogger(discard()))

	// Occupy the only worker so both lanes build up before any dispatch.
	gate := make(chan struct{})
	started := make(chan struct{})
	p.Submit(func() { close(started); <-gate })
	<-started

	const flood = 10
	var mu sync.Mutex
	var order []string
	record := func(k string) {
		mu.Lock()
		order = append(order, k)
		mu.Unlock()
	}
	for i := 0; i < flood; i++ {
		p.SubmitKeyed("a", func() { record("a") })
	}
	p.SubmitKeyed("b", func() { record("b") })

	close(gate)
	deadline := time.After(10 * time.Second)
	for {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n == flood+1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d of %d jobs ran", n, flood+1)
		case <-time.After(5 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if order[0] != "a" || order[1] != "b" {
		t.Fatalf("execution order %v; want a then b — b waited behind a's flood", order)
	}
	for i, k := range order[2:] {
		if k != "a" {
			t.Fatalf("execution order %v: unexpected job %q at %d", order, k, i+2)
		}
	}
}

// TestJobPanicDoesNotKillTheWorker: a panicking job must not shrink the pool.
// A permanently lost worker slows every tenant, silently.
func TestJobPanicDoesNotKillTheWorker(t *testing.T) {
	p := New(1, WithLogger(discard()))
	p.Submit(func() { panic("boom") })
	done := make(chan struct{})
	p.Submit(func() { close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not survive a panicking job")
	}
}

// TestSubmitKeyedBackpressureIsPerKey: a full lane blocks its own submitters
// at the cap, and nobody else. Under the shared buffer, one tenant's flood
// consumed backpressure room for every tenant.
func TestSubmitKeyedBackpressureIsPerKey(t *testing.T) {
	p := New(1, WithLogger(discard()))
	release := make(chan struct{})
	started := make(chan struct{})
	p.SubmitKeyed("a", func() { close(started); <-release })
	<-started // the only worker is occupied by an "a" job

	for i := 0; i < maxQueue; i++ {
		p.SubmitKeyed("a", func() {})
	}
	if got := p.Queued(); got != maxQueue {
		t.Fatalf("queued = %d; want %d", got, maxQueue)
	}

	// The next job for a must park on the full lane.
	submitted := make(chan struct{})
	go func() {
		p.SubmitKeyed("a", func() {})
		close(submitted)
	}()
	select {
	case <-submitted:
		t.Fatal("a submit onto a full lane was accepted")
	case <-time.After(200 * time.Millisecond):
		// Expected: parked.
	}

	// b's lane is a different lane: it accepts and runs despite a's flood.
	bDone := make(chan struct{})
	p.SubmitKeyed("b", func() { close(bDone) })
	close(release)
	select {
	case <-bDone:
	case <-time.After(5 * time.Second):
		t.Fatal("b starved behind a's full lane")
	}

	// Release also lets the parked a-submit through, and the lane drains.
	select {
	case <-submitted:
	case <-time.After(5 * time.Second):
		t.Fatal("the parked submit never proceeded after the lane drained")
	}
	deadline := time.After(5 * time.Second)
	for p.Queued() != 0 {
		select {
		case <-deadline:
			t.Fatalf("backlog never drained: %d jobs left", p.Queued())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
