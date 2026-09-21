package inbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
)

// postgresDSN points these cases at a server:
//
//	AGENTFLOW_TEST_POSTGRES='postgres://…' go test ./internal/core/inbox/
//
// Unset, they skip. Session keys are tagged per run, so the suite is
// re-runnable against a shared server.
var postgresDSN = os.Getenv("AGENTFLOW_TEST_POSTGRES")

func pgSession(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "inbox-test:" + hex.EncodeToString(b)
}

// The claim — an UPDATE whose WHERE clause decides who owns a message, with a
// limited subquery choosing how many — on the backend a fleet runs on. Two
// instances claiming at the same instant must not both be handed the same
// message, and a claim that goes stale must become claimable again.
func TestPostgresClaimIsExclusiveThenStale(t *testing.T) {
	if postgresDSN == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
	}
	ctx := context.Background()
	path := postgresDSN
	a := openInstance(t, path, "instance-a")
	b := openInstance(t, path, "instance-b")
	key := pgSession(t)

	if err := a.Post(ctx, key, msg("m1")); err != nil {
		t.Fatalf("post: %v", err)
	}
	if err := a.Post(ctx, key, msg("m2")); err != nil {
		t.Fatalf("post: %v", err)
	}
	got, err := a.Claim(ctx, "owner-a", key, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("claim on a: %+v err=%v", got, err)
	}
	// b cannot take a fresh claim.
	if other, err := b.Claim(ctx, "owner-b", key, 10); err != nil || len(other) != 0 {
		t.Fatalf("a fresh claim must be respected: %+v err=%v", other, err)
	}
	// Once it goes stale, the messages are b's to deliver.
	b.SetVisibility(0)
	other, err := b.Claim(ctx, "owner-b", key, 10)
	if err != nil || len(other) != 2 {
		t.Fatalf("a stale claim must be recoverable: %+v err=%v", other, err)
	}
	// Only the current owner can close them.
	if err := a.Ack(ctx, "owner-a", other); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if n, _ := a.Depth(ctx, key); n != 2 {
		t.Fatalf("a stale owner's ack must not close anything, depth=%d", n)
	}
	if err := b.Ack(ctx, "owner-b", other); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if n, _ := b.Depth(ctx, key); n != 0 {
		t.Fatalf("the owner's ack should close them, depth=%d", n)
	}
	// An acked message never comes back.
	if again, err := b.Claim(ctx, "owner-b", key, 10); err != nil || len(again) != 0 {
		t.Fatalf("an acked message must not return: %+v err=%v", again, err)
	}
}

// Two instances mint message ids independently, so the same id from both is two
// messages. On the server the primary key has to keep them apart.
func TestPostgresSameIDFromTwoInstancesIsTwoMessages(t *testing.T) {
	if postgresDSN == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
	}
	ctx := context.Background()
	a := openInstance(t, postgresDSN, "instance-a")
	b := openInstance(t, postgresDSN, "instance-b")
	key := pgSession(t)

	if err := a.Post(ctx, key, msg("wh-1")); err != nil {
		t.Fatalf("post on a: %v", err)
	}
	if err := b.Post(ctx, key, msg("wh-1")); err != nil {
		t.Fatalf("post on b: %v", err)
	}
	if n, err := a.Depth(ctx, key); err != nil || n != 2 {
		t.Fatalf("both instances' messages must be queued: n=%d err=%v", n, err)
	}
	// And a repost from one of them is still one row.
	if err := a.Post(ctx, key, msg("wh-1")); err != nil {
		t.Fatalf("repost: %v", err)
	}
	if n, _ := a.Depth(ctx, key); n != 2 {
		t.Fatalf("a repost must not add a row, depth=%d", n)
	}
}

// Order is the order the engine accepted the messages, and the limit bounds how
// many one pass takes.
func TestPostgresClaimOrderAndLimit(t *testing.T) {
	if postgresDSN == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
	}
	ctx := context.Background()
	q := openInstance(t, postgresDSN, "instance-a")
	key := pgSession(t)

	want := []string{"m1", "m2", "m3", "m4", "m5"}
	for _, id := range want {
		if err := q.Post(ctx, key, msg(id)); err != nil {
			t.Fatalf("post %s: %v", id, err)
		}
	}
	first, err := q.Claim(ctx, "owner-a", key, 2)
	if err != nil || len(first) != 2 {
		t.Fatalf("a limited claim should take two: %+v err=%v", first, err)
	}
	if first[0].Message.ID != "m1" || first[1].Message.ID != "m2" {
		t.Fatalf("the earliest messages should come first: %+v", first)
	}
	// The limit is not a lock on the rest: the next pass takes the next batch.
	if err := q.Ack(ctx, "owner-a", first); err != nil {
		t.Fatalf("ack: %v", err)
	}
	rest, err := q.Claim(ctx, "owner-a", key, 10)
	if err != nil || len(rest) != 3 {
		t.Fatalf("the rest should follow: %+v err=%v", rest, err)
	}
	if rest[0].Message.ID != "m3" {
		t.Fatalf("order across passes: got %s", rest[0].Message.ID)
	}
}

// The sweep reclaims delivered rows only.
func TestPostgresSweepOnlyRemovesDeliveredRows(t *testing.T) {
	if postgresDSN == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
	}
	ctx := context.Background()
	q := openInstance(t, postgresDSN, "instance-a")
	q.SetRetention(0)
	key := pgSession(t)

	if err := q.Post(ctx, key, msg("done")); err != nil {
		t.Fatalf("post: %v", err)
	}
	if err := q.Post(ctx, key, msg("pending")); err != nil {
		t.Fatalf("post: %v", err)
	}
	items, err := q.Claim(ctx, "owner-a", key, 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("claim: %+v err=%v", items, err)
	}
	if err := q.Ack(ctx, "owner-a", items[:1]); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// The sweep is fleet-wide by design, so it may reclaim other runs' rows
	// too; what matters is that this run's undelivered row survives.
	if _, err := q.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n, _ := q.Depth(ctx, key); n != 1 {
		t.Fatalf("the undelivered message must survive, depth=%d", n)
	}
	items, err = q.Claim(ctx, "owner-a", key, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("the surviving message must still be claimable: %+v err=%v", items, err)
	}
}
