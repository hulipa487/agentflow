package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"agentflow/internal/core/lease"
)

// housekeepingTTL must outlive the job's own interval, or the lease lapses
// between two ticks and a peer runs the job as well.
func TestHousekeepingTTLOutlivesTheInterval(t *testing.T) {
	if got := housekeepingTTL(time.Hour); got <= time.Hour {
		t.Fatalf("hourly job lease = %v, want more than an hour", got)
	}
	if got := housekeepingTTL(24 * time.Hour); got <= 24*time.Hour {
		t.Fatalf("daily job lease = %v, want more than a day", got)
	}
	if got := housekeepingTTL(0); got != lease.DefaultTTL {
		t.Fatalf("zero interval lease = %v, want the default", got)
	}
}

// TestSecondInstanceSkipsWithinTheInterval: a peer that ticks inside the
// lease's life must not run the job. The owner renewing its own lease still
// runs it, which is how a long-lived instance keeps doing the work.
func TestSecondInstanceSkipsWithinTheInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.db")
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := lease.Open(path, "instance-a", time.Minute, log)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := lease.Open(path, "instance-b", time.Minute, log)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	ttl := housekeepingTTL(time.Hour)
	var ranA, ranB int
	job := func(counter *int) func() { return func() { *counter++ } }

	if !claimAndRun(ctx, a, "daily-job", ttl, log, job(&ranA)) {
		t.Fatal("first instance did not run the job")
	}
	if claimAndRun(ctx, b, "daily-job", ttl, log, job(&ranB)) {
		t.Fatal("second instance ran the job inside the lease")
	}
	// The holder renews with its own acquire, and is allowed to run again.
	if !claimAndRun(ctx, a, "daily-job", ttl, log, job(&ranA)) {
		t.Fatal("the holder was refused a renewal")
	}
	if ranA != 2 || ranB != 0 {
		t.Fatalf("ranA=%d ranB=%d, want 2/0", ranA, ranB)
	}
}
