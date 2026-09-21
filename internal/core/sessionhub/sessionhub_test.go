package sessionhub

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agentflow/internal/core/inbox"
	"agentflow/internal/core/lease"
	"agentflow/internal/core/session"
)

// delivered records what one instance's local supervisor would have received.
type delivered struct {
	mu  sync.Mutex
	ids []string
	err error // when set, every delivery fails
}

func (d *delivered) fn() Deliver {
	return func(_, _ string, msg session.Message) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.err != nil {
			return d.err
		}
		d.ids = append(d.ids, msg.ID)
		return nil
	}
}

func (d *delivered) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.ids...)
}

func (d *delivered) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.ids)
}

// oneInstance builds a hub over the shared store, standing in for one engine.
func oneInstance(t *testing.T, storePath, owner string) (*Hub, *delivered) {
	t.Helper()
	leases, err := lease.Open(storePath, owner, time.Minute, nil)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	t.Cleanup(func() { _ = leases.Close() })
	q, err := inbox.Open(storePath, owner, nil)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	d := &delivered{}
	return New(leases, q, d.fn(), nil), d
}

// twoInstances are two engines sharing one store: the smallest honest model of
// a fleet, and the one that runs anywhere.
func twoInstances(t *testing.T) (a, b *Hub, da, db *delivered) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	a, da = oneInstance(t, path, "instance-a")
	b, db = oneInstance(t, path, "instance-b")
	return a, b, da, db
}

func msg(id string) session.Message {
	return session.Message{ID: id, Type: "text", From: "user:u1", Text: id, Ts: time.Now().Unix()}
}

// The common case costs no extra hop: an instance that can take the session
// delivers the message itself.
func TestFirstMessageIsDeliveredLocally(t *testing.T) {
	a, _, da, _ := twoInstances(t)
	if err := a.Route(context.Background(), "bot", "chat-1", msg("m1")); err != nil {
		t.Fatalf("route: %v", err)
	}
	if da.count() != 1 {
		t.Fatalf("the receiving instance should have delivered it, got %v", da.got())
	}
	// Nothing was queued, so a drain has nothing to do.
	a.drainOnce(context.Background())
	if da.count() != 1 {
		t.Fatalf("a drain must not redeliver: %v", da.got())
	}
}

// A message that arrives on an instance which does not own the session is
// queued, not delivered there — the split conversation this exists to prevent.
func TestMessageForAnotherInstancesSessionIsQueued(t *testing.T) {
	a, b, da, db := twoInstances(t)
	ctx := context.Background()
	// a takes the session.
	if err := a.Route(ctx, "bot", "chat-1", msg("m1")); err != nil {
		t.Fatalf("route on a: %v", err)
	}
	// b receives the next message for it.
	if err := b.Route(ctx, "bot", "chat-1", msg("m2")); err != nil {
		t.Fatalf("route on b: %v", err)
	}
	if db.count() != 0 {
		t.Fatalf("b must not deliver another instance's session, got %v", db.got())
	}
	// The owner picks it up on its next pass.
	a.drainOnce(ctx)
	if got := da.got(); len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Fatalf("the owner should have delivered both in order, got %v", got)
	}
	// And only once.
	a.drainOnce(ctx)
	if da.count() != 2 {
		t.Fatalf("a drain must not redeliver, got %v", da.got())
	}
}

// An instance that dies mid-conversation hands it over, backlog included: the
// messages received while it was the owner are not lost with it.
func TestTakeoverCarriesTheBacklog(t *testing.T) {
	a, b, _, db := twoInstances(t)
	ctx := context.Background()
	if err := a.Route(ctx, "bot", "chat-1", msg("m1")); err != nil {
		t.Fatalf("route on a: %v", err)
	}
	// Two messages arrive at b while a owns the session.
	if err := b.Route(ctx, "bot", "chat-1", msg("m2")); err != nil {
		t.Fatalf("route m2: %v", err)
	}
	if err := b.Route(ctx, "bot", "chat-1", msg("m3")); err != nil {
		t.Fatalf("route m3: %v", err)
	}
	// a goes away for good.
	a.Release(ctx)
	if err := b.Route(ctx, "bot", "chat-1", msg("m4")); err != nil {
		t.Fatalf("route m4: %v", err)
	}
	// b now owns it, and the messages a never delivered are waiting.
	b.drainOnce(ctx)
	if got := db.got(); len(got) != 3 {
		t.Fatalf("the takeover should carry the backlog, got %v", got)
	}
}

// A delivery that fails locally is not a lost message: the engine already
// accepted it, so the owner queues it and the drain retries. Reporting an error
// would invite the caller to retry a message the queue has already covered.
func TestFailedDeliveryIsQueuedNotLost(t *testing.T) {
	a, _, da, _ := twoInstances(t)
	ctx := context.Background()
	da.err = errors.New("session actor is not accepting messages")
	if err := a.Route(ctx, "bot", "chat-1", msg("m1")); err != nil {
		t.Fatalf("a message the engine accepted must not report failure: %v", err)
	}
	if da.count() != 0 {
		t.Fatalf("nothing should have been delivered, got %v", da.got())
	}
	// The actor recovers; the next drain delivers what was queued.
	da.err = nil
	a.drainOnce(ctx)
	if got := da.got(); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("the queued message should be delivered on the next pass, got %v", got)
	}
}

// Message ids are minted per instance — a channel driver counts its own
// messages — so the same id arriving from two instances is two turns, not one.
// Folding them together would silently drop a message a person sent.
func TestSameIDFromTwoInstancesIsTwoTurns(t *testing.T) {
	a, b, da, _ := twoInstances(t)
	ctx := context.Background()
	if err := a.Route(ctx, "bot", "chat-1", msg("wh-1")); err != nil {
		t.Fatalf("route on a: %v", err)
	}
	if err := b.Route(ctx, "bot", "chat-1", msg("wh-1")); err != nil {
		t.Fatalf("route on b: %v", err)
	}
	a.drainOnce(ctx)
	if got := da.got(); len(got) != 2 {
		t.Fatalf("both instances' messages must be delivered, got %v", got)
	}
}

// Posting the same message twice is one row: a retried inbound cannot become a
// duplicate turn.
func TestRepostingTheSameMessageIsOneRow(t *testing.T) {
	a, b, da, _ := twoInstances(t)
	ctx := context.Background()
	if err := a.Route(ctx, "bot", "chat-1", msg("m1")); err != nil {
		t.Fatalf("route on a: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := b.Route(ctx, "bot", "chat-1", msg("m2")); err != nil {
			t.Fatalf("repost %d: %v", i, err)
		}
	}
	a.drainOnce(ctx)
	if got := da.got(); len(got) != 2 {
		t.Fatalf("a reposted message must be delivered once, got %v", got)
	}
}

// Sessions are independent: owning one is not owning another, so a fleet
// spreads conversations across instances instead of pinning them all to the
// instance that booted first.
func TestSessionsAreClaimedIndependently(t *testing.T) {
	a, b, da, db := twoInstances(t)
	ctx := context.Background()
	if err := a.Route(ctx, "bot", "chat-a", msg("a1")); err != nil {
		t.Fatalf("route a: %v", err)
	}
	if err := b.Route(ctx, "bot", "chat-b", msg("b1")); err != nil {
		t.Fatalf("route b: %v", err)
	}
	if da.count() != 1 || db.count() != 1 {
		t.Fatalf("each instance should own its own session: a=%v b=%v", da.got(), db.got())
	}
	if a.Held() != 1 || b.Held() != 1 {
		t.Fatalf("held sessions: a=%d b=%d", a.Held(), b.Held())
	}
}

// Messages within one session are delivered in the order they were accepted.
func TestBacklogKeepsOrder(t *testing.T) {
	a, b, da, _ := twoInstances(t)
	ctx := context.Background()
	if err := a.Route(ctx, "bot", "chat-1", msg("first")); err != nil {
		t.Fatalf("route: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := b.Route(ctx, "bot", "chat-1", msg(fmt.Sprintf("q%d", i))); err != nil {
			t.Fatalf("route %d: %v", i, err)
		}
	}
	a.drainOnce(ctx)
	want := []string{"first", "q0", "q1", "q2", "q3", "q4"}
	got := da.got()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: got %v, want %v", got, want)
		}
	}
}

// A message with no id is refused rather than delivered: without one there is
// nothing to deduplicate a redelivery against.
func TestMessageWithoutAnIDIsRefused(t *testing.T) {
	a, _, _, _ := twoInstances(t)
	if err := a.Route(context.Background(), "bot", "chat-1", session.Message{Type: "text"}); err == nil {
		t.Fatal("a message without an id must be refused")
	}
}

// The session key has to be exactly what the supervisor keys its actors by, or
// the hub and the session map would disagree about which session a message
// belongs to.
// A claim is what makes a daemon boot once per deployment rather than once per
// instance: the claimant owns the session from then on — the drain renews it and
// delivers its traffic — and a peer is told no. It is ownership, not a lock.
func TestClaimGivesTheSessionToOneInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	a, aDelivered := oneInstance(t, path, "instance-a")
	b, _ := oneInstance(t, path, "instance-b")
	ctx := context.Background()

	ok, err := a.Claim(ctx, "daemon|boot")
	if err != nil || !ok {
		t.Fatalf("the first claim must win: ok=%v err=%v", ok, err)
	}
	if ok, err := a.Claim(ctx, "daemon|boot"); err != nil || !ok {
		t.Fatalf("the owner must be able to renew its own claim: ok=%v err=%v", ok, err)
	}
	if ok, err := b.Claim(ctx, "daemon|boot"); err != nil || ok {
		t.Fatalf("a peer must not take a claimed session: ok=%v err=%v", ok, err)
	}

	// The claim shows up as ownership, so the drain renews it and picks up what
	// is queued for the session — a peer's message reaches the owner.
	if a.Held() != 1 {
		t.Fatalf("a claimed session must count as held, held=%d", a.Held())
	}
	if err := b.Route(ctx, "daemon", "boot", session.Message{ID: "m1", Type: "user", Ts: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	a.drainOnce(ctx)
	if got := aDelivered.got(); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("the owner did not drain the session it claimed: %v", got)
	}
}

func TestSessionKeyRoundTrip(t *testing.T) {
	key := SessionKey("bot", "telegram:42")
	if key != "bot|telegram:42" {
		t.Fatalf("session key = %q", key)
	}
	agent, k, ok := SplitSessionKey(key)
	if !ok || agent != "bot" || k != "telegram:42" {
		t.Fatalf("split = %q %q %v", agent, k, ok)
	}
	if _, _, ok := SplitSessionKey("nodivider"); ok {
		t.Fatal("a key without a divider is not a session key")
	}
}
