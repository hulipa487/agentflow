package identity

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Two registries over one store are two instances of the engine sharing a
// database: the smallest honest model of a fleet, and the one these tests use
// because it runs anywhere — no server, no second machine.
func twoInstances(t *testing.T) (a, b *Registry, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "identity.db")
	a = openAt(t, path)
	b = openAt(t, path)
	return a, b, path
}

func openAt(t *testing.T, path string) *Registry {
	t.Helper()
	r, err := Open(path, testLogger())
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// A handle first seen by one instance resolves to the *same* identity on
// another. Two instances minting their own identity for one handle would split
// a person in half the moment they were load-balanced to the other one.
func TestFleetFirstContactAdoptsTheOtherInstancesRow(t *testing.T) {
	a, b, _ := twoInstances(t)

	x, err := a.Resolve("telegram", "tg:42", "chat-a", map[string]any{"username": "oscar"})
	if err != nil {
		t.Fatalf("resolve on a: %v", err)
	}
	y, err := b.Resolve("telegram", "tg:42", "chat-b", nil)
	if err != nil {
		t.Fatalf("resolve on b: %v", err)
	}
	if x.IdentityID != y.IdentityID {
		t.Fatalf("one handle must be one identity across instances: %q vs %q", x.IdentityID, y.IdentityID)
	}
	if y.UserID != x.UserID {
		t.Fatalf("user scope disagrees across instances: %q vs %q", x.UserID, y.UserID)
	}
	if n := countRows(t, a, `SELECT COUNT(*) FROM identities WHERE native_from = ?`, "tg:42"); n != 1 {
		t.Fatalf("expected exactly one identity row, got %d", n)
	}
}

// instances opens n registries over one store: n engines sharing a database.
func instances(t *testing.T, n int) []*Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "identity.db")
	out := make([]*Registry, n)
	for i := range out {
		out[i] = openAt(t, path)
	}
	return out
}

// holdAtGate parks every caller inside the mint window until want of them have
// arrived, then releases them together. It turns the cross-instance mint race
// into something reproducible rather than a matter of scheduling luck — without
// it a test of this race passes whether or not the code survives it.
func holdAtGate(t *testing.T, want int) {
	t.Helper()
	var mu sync.Mutex
	var once sync.Once
	arrived := 0
	release := make(chan struct{})
	open := func() { once.Do(func() { close(release) }) }
	mintGate = func() {
		mu.Lock()
		arrived++
		full := arrived >= want
		mu.Unlock()
		if full {
			open()
		}
		<-release
	}
	// A bound, so a test that never assembles its full cast fails visibly
	// instead of hanging.
	go func() {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			open()
		}
	}()
	t.Cleanup(func() { mintGate = nil })
}

// One handle's first message arrives at every instance at the same moment: one
// insert wins, the rest adopt it. Without the conflict-safe insert the losers
// fail on the unique handle — a first contact that is an error, not a welcome.
func TestFleetConcurrentFirstContactMintsOnce(t *testing.T) {
	const n = 4
	regs := instances(t, n)
	holdAtGate(t, n)

	var wg sync.WaitGroup
	got := make([]Resolution, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = regs[i].Resolve("telegram", "tg:race", "chat", nil)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("instance %d failed a first contact: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if got[i].IdentityID != got[0].IdentityID {
			t.Fatalf("instances disagree on the identity: %q vs %q", got[i].IdentityID, got[0].IdentityID)
		}
	}
	if count := countRows(t, regs[0], `SELECT COUNT(*) FROM identities WHERE native_from = ?`, "tg:race"); count != 1 {
		t.Fatalf("concurrent first contact minted %d rows, want 1", count)
	}
}

// The same race with auto-claiming on: exactly one account, and it is the one
// the surviving identity points at. A lost race that still claimed a profile
// would leave an account nothing can reach.
func TestFleetConcurrentAutoClaimMakesOneProfile(t *testing.T) {
	const n = 4
	regs := instances(t, n)
	for _, r := range regs {
		r.SetAutoClaim(true)
	}
	holdAtGate(t, n)

	var wg sync.WaitGroup
	for i, r := range regs {
		wg.Add(1)
		go func(i int, r *Registry) {
			defer wg.Done()
			if _, err := r.Resolve("telegram", "tg:claim", "chat", nil); err != nil {
				t.Errorf("instance %d: %v", i, err)
			}
		}(i, r)
	}
	wg.Wait()

	if count := countRows(t, regs[0], `SELECT COUNT(*) FROM identities WHERE native_from = ?`, "tg:claim"); count != 1 {
		t.Fatalf("expected one identity, got %d", count)
	}
	if count := countRows(t, regs[0], `SELECT COUNT(*) FROM profiles`); count != 1 {
		t.Fatalf("expected exactly one claimed profile, got %d", count)
	}
	if count := countRows(t, regs[0], `SELECT COUNT(*) FROM identities WHERE user_id != ''`); count != 1 {
		t.Fatalf("the identity must be linked to the claimed profile, got %d linked rows", count)
	}
}

// The storage property the two tests above depend on, stated directly: a second
// writer for a handle that already exists reports "not mine" rather than
// failing. A unique-constraint error here would surface as a failed first
// contact in whichever instance lost.
func TestInsertIdentityReportsTheLoser(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	now := time.Now().Unix()
	mine := Identity{ID: "i_mine", Channel: "telegram", NativeFrom: "tg:x", Trust: TrustVerified, FirstSeen: now, LastSeen: now}
	theirs := Identity{ID: "i_theirs", Channel: "telegram", NativeFrom: "tg:x", Trust: TrustVerified, FirstSeen: now, LastSeen: now}

	won, err := r.insertIdentity(ctx, mine)
	if err != nil || !won {
		t.Fatalf("first insert: won=%v err=%v", won, err)
	}
	won, err = r.insertIdentity(ctx, theirs)
	if err != nil {
		t.Fatalf("a losing insert must not be an error: %v", err)
	}
	if won {
		t.Fatal("the second insert must report that it created nothing")
	}
	got, ok, err := r.lookupIdentity("tg:x")
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.ID != "i_mine" {
		t.Fatalf("the first row must survive, got %q", got.ID)
	}
}

// A link made on one instance applies to the next inbound on any other. This is
// what a per-process resolution cache used to break: the instance that had
// already seen the handle kept serving the scope it cached before the link.
func TestFleetLinkAndUnlinkAreVisibleEverywhere(t *testing.T) {
	a, b, _ := twoInstances(t)
	p, err := a.CreateProfile("Oscar", "")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	// The handle has to have written in before it can be linked.
	if _, err := a.Resolve("telegram", "tg:7", "chat", nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// b resolves it while still unlinked — with a cache, this is the answer it
	// would keep.
	if res, err := b.Resolve("telegram", "tg:7", "chat", nil); err != nil {
		t.Fatalf("resolve on b: %v", err)
	} else if res.Registered() {
		t.Fatalf("handle should start unregistered, got %q", res.UserID)
	}

	if _, err := a.Link(p.UserID, "tg:7"); err != nil {
		t.Fatalf("link: %v", err)
	}
	res, err := b.Resolve("telegram", "tg:7", "chat", nil)
	if err != nil {
		t.Fatalf("resolve after link: %v", err)
	}
	if !res.Registered() || res.UserID != p.UserID {
		t.Fatalf("a link made on one instance must hold on every other, got %q want %q", res.UserID, p.UserID)
	}

	// Unlink is the security-relevant direction: once it happens, traffic from
	// the handle must stop carrying the person's scope — on every instance.
	// A profile's last handle cannot be unlinked, so link a second one first.
	if _, err := a.Resolve("telegram", "tg:8", "chat2", nil); err != nil {
		t.Fatalf("resolve second handle: %v", err)
	}
	if _, err := a.Link(p.UserID, "tg:8"); err != nil {
		t.Fatalf("link second handle: %v", err)
	}
	// b resolves it *while linked*, which is the answer it would keep if it
	// cached one.
	linked, err := b.Resolve("telegram", "tg:8", "chat2", nil)
	if err != nil {
		t.Fatalf("resolve linked handle: %v", err)
	}
	if linked.UserID != p.UserID {
		t.Fatalf("second handle should be linked, got %q", linked.UserID)
	}
	if err := a.Unlink(p.UserID, linked.IdentityID); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	res2, err := b.Resolve("telegram", "tg:8", "chat2", nil)
	if err != nil {
		t.Fatalf("resolve after unlink: %v", err)
	}
	if res2.Registered() {
		t.Fatalf("an unlinked handle must carry no scope on any instance, got %q", res2.UserID)
	}
}

// Channel traits are a deployment setting, and every instance heals the rows it
// finds with them. The property that matters is that the *answer* follows the
// traits, on whichever instance asks.
func TestFleetTraitsApplyToStoredRows(t *testing.T) {
	a, b, _ := twoInstances(t)
	// First contact under the conservative default: an unnamed channel is
	// asserted, undeliverable and unlinkable.
	res, err := a.Resolve("tg-main", "tg:9", "chat", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Linkable || res.Trust != TrustAsserted {
		t.Fatalf("an unknown channel must start unlinkable/asserted, got %+v", res)
	}
	// The deployment names its telegram channel; drivers report that name.
	if err := a.SetChannelTraits(map[string]ChannelTraits{"tg-main": Traits("telegram")}); err != nil {
		t.Fatalf("set traits: %v", err)
	}
	res, err = b.Resolve("tg-main", "tg:9", "chat", nil)
	if err != nil {
		t.Fatalf("resolve after traits: %v", err)
	}
	if !res.Linkable || res.Trust != TrustVerified {
		t.Fatalf("traits must apply to an existing row on any instance, got %+v", res)
	}
}

// A handle that arrives on two instances at once, with different delivery
// targets, ends up with one row and the newer target — not a duplicate or a
// lost reply address.
func TestFleetLastWriterSetsTheDeliveryTarget(t *testing.T) {
	a, b, _ := twoInstances(t)
	if _, err := a.Resolve("telegram", "tg:11", "chat-old", nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	id, ok, err := a.lookupIdentity("tg:11")
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	// The target changed, so the write is immediate even inside the throttle
	// window.
	if _, err := b.Resolve("telegram", "tg:11", "chat-new", nil); err != nil {
		t.Fatalf("resolve on b: %v", err)
	}
	got, ok, err := a.lookupIdentity("tg:11")
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.ID != id.ID {
		t.Fatalf("the row must not be replaced: %q vs %q", got.ID, id.ID)
	}
	if got.ReplyTo != "chat-new" {
		t.Fatalf("reply target = %q, want the most recent one", got.ReplyTo)
	}
}

// Linking a handle that another instance linked first is refused rather than
// silently stolen. The API-level refusal reads the row first; the statements
// underneath are the guarantee, so both are asserted here.
func TestFleetLinkLosesTheRaceCleanly(t *testing.T) {
	a, b, _ := twoInstances(t)
	first, _ := a.CreateProfile("First", "")
	second, _ := b.CreateProfile("Second", "")
	if _, err := a.Resolve("telegram", "tg:3", "chat", nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := a.Link(first.UserID, "tg:3"); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := b.Link(second.UserID, "tg:3"); err == nil {
		t.Fatal("a handle already linked to a profile must not be linked to another")
	}

	// The case the API check cannot cover: b read the handle as unlinked, and a
	// linked it before b's write. The predicate in the statement is what makes
	// that stale read harmless.
	id, _, err := a.lookupIdentity("tg:3")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	assigned, err := b.assignIdentity(context.Background(), second.UserID, id.ID)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if assigned {
		t.Fatal("a stale read must not overwrite a live link")
	}
	if id.UserID != first.UserID {
		t.Fatalf("the original link must survive, got %q want %q", id.UserID, first.UserID)
	}
	// And the mirror image: a profile that does not hold the handle cannot take
	// it away, however stale its view.
	released, err := b.releaseIdentity(context.Background(), second.UserID, id.ID)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released {
		t.Fatal("a handle must not be released by a profile that does not hold it")
	}
	if released, err := a.releaseIdentity(context.Background(), first.UserID, id.ID); err != nil || !released {
		t.Fatalf("the holder must be able to release it: released=%v err=%v", released, err)
	}
	if got, _, _ := a.lookupIdentity("tg:3"); got.UserID != "" {
		t.Fatalf("the handle should be unlinked, got %q", got.UserID)
	}
}

// The store is shared, so an account made through the API on one instance is
// the account the next inbound on another carries.
func TestFleetProvisionedLoginIsShared(t *testing.T) {
	a, b, _ := twoInstances(t)
	id, created, err := a.ProvisionOIDC("https://issuer|sub-1", "Oscar", "o@example.com", true)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !created {
		t.Fatal("first login should provision")
	}
	again, created, err := b.ProvisionOIDC("https://issuer|sub-1", "Oscar", "o@example.com", true)
	if err != nil {
		t.Fatalf("provision on b: %v", err)
	}
	if created {
		t.Fatal("the second instance must not create a second account for one login")
	}
	if again.ID != id.ID || again.UserID != id.UserID {
		t.Fatalf("login resolved differently across instances: %+v vs %+v", again, id)
	}
	if n := countRows(t, a, `SELECT COUNT(*) FROM profiles`); n != 1 {
		t.Fatalf("expected one profile, got %d", n)
	}
}

// last_seen is throttled — it is what push ordering reads, but writing it on
// every inbound makes one row the hot row of a busy chat. A changed reply
// target still writes immediately.
func TestRefreshThrottlesLastSeen(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	if _, err := r.Resolve("telegram", "tg:20", "chat", nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	id, _, err := r.lookupIdentity("tg:20")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	// Backdate the row: the throttle only skips a write inside its window.
	if _, err := r.st.Exec(ctx, `UPDATE identities SET last_seen = ? WHERE id = ?`,
		time.Now().Add(-2*time.Hour).Unix(), id.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := r.Resolve("telegram", "tg:20", "chat", nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	after, _, err := r.lookupIdentity("tg:20")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if after.LastSeen <= id.LastSeen-3600 {
		t.Fatalf("a stale last_seen must be refreshed, got %d (was %d)", after.LastSeen, id.LastSeen)
	}
	// Inside the window, with nothing changed, the row is left alone.
	stamp := after.LastSeen
	if _, err := r.Resolve("telegram", "tg:20", "chat", nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	same, _, _ := r.lookupIdentity("tg:20")
	if same.LastSeen != stamp {
		t.Fatalf("last_seen rewritten inside the window: %d -> %d", stamp, same.LastSeen)
	}
	// A new reply target is not throttled: delivery must not go stale.
	if _, err := r.Resolve("telegram", "tg:20", "chat-moved", nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	moved, _, _ := r.lookupIdentity("tg:20")
	if moved.ReplyTo != "chat-moved" {
		t.Fatalf("reply target = %q, want the new one", moved.ReplyTo)
	}
}

func countRows(t *testing.T, r *Registry, query string, args ...any) int {
	t.Helper()
	var n int
	if err := r.st.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}
