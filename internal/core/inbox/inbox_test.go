package inbox

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agentflow/internal/core/session"
)

func openInstance(t *testing.T, path, origin string) *Queue {
	t.Helper()
	q, err := Open(path, origin, nil)
	if err != nil {
		t.Fatalf("open %s: %v", origin, err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func msg(id string) session.Message {
	return session.Message{ID: id, Type: "text", From: "user:u1", Text: id, Ts: time.Now().Unix()}
}

// Two instances write to one inbox. The ids they mint are their own — a channel
// driver's counter restarts at one on every instance — so the same id on two of
// them is two messages, and folding them together would silently drop a turn.
// Pending answers "which of these sessions has something waiting" in one query,
// and claims nothing: the answer decides what a drain pass looks at, while the
// claim that follows is what hands a message to exactly one instance.
func TestPendingNamesOnlySessionsWithWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	a := openInstance(t, path, "instance-a")
	b := openInstance(t, path, "instance-b")
	ctx := context.Background()

	if err := a.Post(ctx, "bot|one", msg("m1")); err != nil {
		t.Fatal(err)
	}
	if err := a.Post(ctx, "bot|three", msg("m3")); err != nil {
		t.Fatal(err)
	}
	// A claim this owner holds unacked is still work: a delivery that crashed
	// before its ack has to be found again, or the message is stuck.
	if _, err := a.Claim(ctx, "instance-a", "bot|three", 10); err != nil {
		t.Fatal(err)
	}
	// An acked message is not work.
	if err := a.Post(ctx, "bot|done", msg("m4")); err != nil {
		t.Fatal(err)
	}
	done, err := a.Claim(ctx, "instance-a", "bot|done", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Ack(ctx, "instance-a", done); err != nil {
		t.Fatal(err)
	}

	got, err := a.Pending(ctx, "instance-a", []string{"bot|one", "bot|two", "bot|three", "bot|done"})
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	want := map[string]bool{"bot|one": true, "bot|three": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Fatalf("pending = %v, want bot|one and bot|three", got)
	}

	// Nothing was claimed on the way: a peer can still take what is waiting.
	items, err := b.Claim(ctx, "instance-b", "bot|one", 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("a peer could not claim what pending only reported: %d items, err=%v", len(items), err)
	}
	// And a session list that is empty is not a query.
	if got, err := a.Pending(ctx, "instance-a", nil); err != nil || got != nil {
		t.Fatalf("empty list = %v, err=%v", got, err)
	}
}

func TestSameIDFromTwoInstancesIsTwoMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	a := openInstance(t, path, "instance-a")
	b := openInstance(t, path, "instance-b")
	ctx := context.Background()

	if err := a.Post(ctx, "bot|chat-1", msg("wh-1")); err != nil {
		t.Fatalf("post on a: %v", err)
	}
	if err := b.Post(ctx, "bot|chat-1", msg("wh-1")); err != nil {
		t.Fatalf("post on b: %v", err)
	}
	if n, err := a.Depth(ctx, "bot|chat-1"); err != nil || n != 2 {
		t.Fatalf("two instances' messages must both be queued: n=%d err=%v", n, err)
	}
}

// One instance posting the same message twice is one row: a retried inbound
// must not become a duplicate turn.
func TestRepostIsOneRow(t *testing.T) {
	q := openInstance(t, filepath.Join(t.TempDir(), "inbox.db"), "instance-a")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := q.Post(ctx, "bot|chat-1", msg("m1")); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	if n, err := q.Depth(ctx, "bot|chat-1"); err != nil || n != 1 {
		t.Fatalf("a repost must be one row: n=%d err=%v", n, err)
	}
}

// A claim is exclusive while it is fresh, and taken over once it goes stale —
// which is how a holder that died mid-delivery is recovered.
func TestClaimIsExclusiveThenStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	a := openInstance(t, path, "instance-a")
	b := openInstance(t, path, "instance-b")
	ctx := context.Background()
	if err := a.Post(ctx, "bot|chat-1", msg("m1")); err != nil {
		t.Fatalf("post: %v", err)
	}

	got, err := a.Claim(ctx, "owner-a", "bot|chat-1", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("claim on a: %+v err=%v", got, err)
	}
	if got[0].Message.Text != "m1" {
		t.Fatalf("the body did not round-trip: %+v", got[0])
	}
	// b cannot take a fresh claim.
	if other, err := b.Claim(ctx, "owner-b", "bot|chat-1", 10); err != nil || len(other) != 0 {
		t.Fatalf("a fresh claim must be respected: %+v err=%v", other, err)
	}
	// Once the claim is stale, the message is b's to deliver.
	b.SetVisibility(0)
	other, err := b.Claim(ctx, "owner-b", "bot|chat-1", 10)
	if err != nil || len(other) != 1 {
		t.Fatalf("a stale claim must be recoverable: %+v err=%v", other, err)
	}
	// Only the current owner can close it.
	if err := a.Ack(ctx, "owner-a", other); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if n, _ := a.Depth(ctx, "bot|chat-1"); n != 1 {
		t.Fatalf("a stale owner's ack must not close the message, depth=%d", n)
	}
	if err := b.Ack(ctx, "owner-b", other); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if n, _ := b.Depth(ctx, "bot|chat-1"); n != 0 {
		t.Fatalf("the owner's ack should close it, depth=%d", n)
	}
}

// An acked row is never claimable again — which is what makes delivery
// at-most-once in the absence of a crash, rather than repeatedly redelivered.
func TestAckedMessagesAreNotRedelivered(t *testing.T) {
	q := openInstance(t, filepath.Join(t.TempDir(), "inbox.db"), "instance-a")
	ctx := context.Background()
	if err := q.Post(ctx, "bot|chat-1", msg("m1")); err != nil {
		t.Fatalf("post: %v", err)
	}
	items, err := q.Claim(ctx, "owner-a", "bot|chat-1", 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim: %+v err=%v", items, err)
	}
	if err := q.Ack(ctx, "owner-a", items); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if again, err := q.Claim(ctx, "owner-a", "bot|chat-1", 10); err != nil || len(again) != 0 {
		t.Fatalf("an acked message must not come back: %+v err=%v", again, err)
	}
}

// Order within a session is the order the engine accepted the messages.
func TestClaimKeepsOrder(t *testing.T) {
	q := openInstance(t, filepath.Join(t.TempDir(), "inbox.db"), "instance-a")
	ctx := context.Background()
	want := []string{"m1", "m2", "m3", "m4"}
	for _, id := range want {
		if err := q.Post(ctx, "bot|chat-1", msg(id)); err != nil {
			t.Fatalf("post %s: %v", id, err)
		}
	}
	items, err := q.Claim(ctx, "owner-a", "bot|chat-1", 10)
	if err != nil || len(items) != len(want) {
		t.Fatalf("claim: %+v err=%v", items, err)
	}
	for i, id := range want {
		if items[i].Message.ID != id {
			t.Fatalf("order: got %s at %d, want %s", items[i].Message.ID, i, id)
		}
	}
}

// The sweep only reclaims acked rows: a message that was never delivered is
// not something a housekeeping pass may delete.
func TestSweepOnlyRemovesDeliveredRows(t *testing.T) {
	q := openInstance(t, filepath.Join(t.TempDir(), "inbox.db"), "instance-a")
	q.SetRetention(0)
	ctx := context.Background()
	if err := q.Post(ctx, "bot|chat-1", msg("done")); err != nil {
		t.Fatalf("post: %v", err)
	}
	if err := q.Post(ctx, "bot|chat-1", msg("pending")); err != nil {
		t.Fatalf("post: %v", err)
	}
	items, err := q.Claim(ctx, "owner-a", "bot|chat-1", 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("claim: %+v err=%v", items, err)
	}
	if err := q.Ack(ctx, "owner-a", items[:1]); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if n, err := q.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep should reclaim exactly the delivered row: n=%d err=%v", n, err)
	}
	if n, _ := q.Depth(ctx, "bot|chat-1"); n != 1 {
		t.Fatalf("the undelivered message must survive, depth=%d", n)
	}
}
