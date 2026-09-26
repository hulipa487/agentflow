package limits

import (
	"testing"
	"time"
)

func TestRateAllowsUpToTheLimit(t *testing.T) {
	r := &Rate{Limit: 3, Window: time.Second}
	for i := 0; i < 3; i++ {
		if !r.Allow("tenant-a") {
			t.Fatalf("call %d of 3 denied", i+1)
		}
	}
	if r.Allow("tenant-a") {
		t.Fatal("call 4 was allowed past the limit")
	}
	// Another tenant's window is its own — the whole point of keying.
	if !r.Allow("tenant-b") {
		t.Fatal("tenant-b was denied while tenant-a was at its own limit")
	}
	// Engine-internal traffic (no tenant) is never limited.
	if !r.Allow("") {
		t.Fatal("internal traffic was limited")
	}
}

func TestRateWindowRollsOver(t *testing.T) {
	r := &Rate{Limit: 1, Window: 50 * time.Millisecond}
	if !r.Allow("tenant-a") {
		t.Fatal("first call denied")
	}
	if r.Allow("tenant-a") {
		t.Fatal("second call inside the window was allowed")
	}
	time.Sleep(60 * time.Millisecond)
	if !r.Allow("tenant-a") {
		t.Fatal("the window never rolled over")
	}
}

func TestRateZeroLimitDisables(t *testing.T) {
	r := &Rate{}
	for i := 0; i < 1000; i++ {
		if !r.Allow("tenant-a") {
			t.Fatal("a zero limit denied traffic; the limiter must be opt-in per bound")
		}
	}
}
