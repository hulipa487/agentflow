package request

import (
	"context"
	"testing"
	"time"

	"agentflow/internal/core/session"
)

func TestResolveDeliversOnce(t *testing.T) {
	r := New()
	id, wait, _ := r.Open("owner-a", "worker")
	msg := session.Message{ID: "reply", Type: "agent"}
	if r.Resolve(id, "intruder", msg) {
		t.Fatal("unexpectedly accepted a reply from another agent")
	}
	if !r.Resolve(id, "worker", msg) {
		t.Fatal("expected intended recipient resolution to succeed")
	}
	if r.Resolve(id, "worker", msg) {
		t.Fatal("expected duplicate resolution to fail")
	}
	got, err := r.Wait(context.Background(), id, wait)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "reply" {
		t.Fatalf("got %q, want reply", got.ID)
	}
}

func TestWaitTimesOutAndCleansUp(t *testing.T) {
	r := New()
	id, wait, _ := r.Open("owner-a", "worker")
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := r.Wait(ctx, id, wait); err == nil {
		t.Fatal("expected timeout")
	}
	if r.Pending() != 0 {
		t.Fatalf("pending=%d, want 0", r.Pending())
	}
}

func TestCancelOwner(t *testing.T) {
	r := New()
	r.Open("owner-a", "worker-a")
	r.Open("owner-b", "worker-b")
	r.CancelOwner("owner-a")
	if r.Pending() != 1 {
		t.Fatalf("pending=%d, want 1", r.Pending())
	}
}
