package identity

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agentflow/internal/core/metrics"
	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "identity.db"), testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// --- resolution -------------------------------------------------------------

func TestResolveMintsOnce(t *testing.T) {
	r := newTestRegistry(t)
	a, err := r.Resolve("telegram", "user:telegram:123", "12345", map[string]any{"username": "oscar"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	b, err := r.Resolve("telegram", "user:telegram:123", "99999", nil)
	if err != nil {
		t.Fatalf("resolve2: %v", err)
	}
	if a.IdentityID != b.IdentityID {
		t.Fatalf("same native_from must resolve to same identity: %q vs %q", a.IdentityID, b.IdentityID)
	}
	if a.IdentityID == "" {
		t.Fatal("identity id empty")
	}
}

// An unlinked handle carries no user scope: that is what keeps an unclaimed
// handle from accumulating personal history, memory or files.
func TestResolveUnregisteredCarriesNoUserScope(t *testing.T) {
	r := newTestRegistry(t)
	res, err := r.Resolve("telegram", "user:telegram:123", "12345", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Registered() {
		t.Fatalf("unlinked handle must not carry a user scope, got %q", res.UserID)
	}
	if res.Trust != TrustVerified {
		t.Errorf("telegram must be trusted-verified, got %q", res.Trust)
	}
	if !res.Deliverable || !res.Linkable {
		t.Errorf("telegram must be deliverable+linkable, got deliverable=%v linkable=%v", res.Deliverable, res.Linkable)
	}
	// An identity id is minted regardless, so the audit trail stays uniform.
	if !strings.HasPrefix(res.IdentityID, "i_") {
		t.Errorf("identity id shape: %q", res.IdentityID)
	}
}

// Asserted channels get no personal scope and cannot be linked.
func TestAssertedChannelIsNotLinkable(t *testing.T) {
	r := newTestRegistry(t)
	res, err := r.Resolve("webhook", "user:webhook:alice", "req-1", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Trust != TrustAsserted || res.Linkable || res.Deliverable {
		t.Fatalf("webhook traits wrong: trust=%q linkable=%v deliverable=%v", res.Trust, res.Linkable, res.Deliverable)
	}
	// An unknown channel is treated as the conservative case.
	if tr := Traits("something-new"); tr.Trust != TrustAsserted || tr.Linkable || tr.Deliverable {
		t.Fatalf("unknown channel traits: %+v", tr)
	}
}

// The auto-claim escape hatch restores pre-registration behavior.
func TestAutoClaimGivesPersonalScope(t *testing.T) {
	r := newTestRegistry(t)
	r.SetAutoClaim(true)
	res, err := r.Resolve("telegram", "user:telegram:123", "12345", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Registered() || !strings.HasPrefix(res.UserID, "u_") {
		t.Fatalf("auto-claim must produce a profile id, got %q", res.UserID)
	}
	p, ok, err := r.Get(res.UserID)
	if err != nil || !ok {
		t.Fatalf("auto-claimed profile not readable: ok=%v err=%v", ok, err)
	}
	if len(p.Identities) != 1 || p.Identities[0].ID != res.IdentityID {
		t.Fatalf("auto-claimed profile should own the identity: %+v", p.Identities)
	}
}

func TestResolveDistinctNativesDistinctIdentities(t *testing.T) {
	r := newTestRegistry(t)
	a, _ := r.Resolve("telegram", "user:telegram:1", "1", nil)
	b, _ := r.Resolve("telegram", "user:telegram:2", "2", nil)
	if a.IdentityID == b.IdentityID {
		t.Fatal("different native_from must yield different identities")
	}
}

func TestResolveCountsMints(t *testing.T) {
	r := newTestRegistry(t)
	c, ok := metrics.Global().Get("agentflow_identity_mints")
	if !ok {
		t.Fatal("agentflow_identity_mints is not registered")
	}
	before := c.Value()

	if _, err := r.Resolve("telegram", "user:telegram:777", "777", nil); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if got := c.Value(); got != before+1 {
		t.Errorf("first contact should count one mint: before=%d after=%d", before, got)
	}
	if _, err := r.Resolve("telegram", "user:telegram:777", "777", nil); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got := c.Value(); got != before+1 {
		t.Errorf("resolving a known handle must not count a mint: %d", got)
	}
}

func TestResolveConcurrentFirstContactMintsOne(t *testing.T) {
	r := newTestRegistry(t)
	const n = 50
	var wg sync.WaitGroup
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := r.Resolve("telegram", "user:telegram:999", "999", nil)
			if err != nil {
				t.Errorf("resolve: %v", err)
				return
			}
			ids[i] = res.IdentityID
		}(i)
	}
	wg.Wait()
	first := ids[0]
	if first == "" {
		t.Fatal("identity id empty")
	}
	for i, id := range ids {
		if id != first {
			t.Fatalf("goroutine %d got %q, want %q — concurrent mint produced duplicate", i, id, first)
		}
	}
}

func TestRefreshUpdatesDeliveryTarget(t *testing.T) {
	r := newTestRegistry(t)
	res, _ := r.Resolve("telegram", "user:telegram:5", "chat-1", nil)
	ch, rt, ok := r.LookupUser(res.IdentityID)
	if !ok || ch != "telegram" || rt != "chat-1" {
		t.Fatalf("lookup after first resolve: ch=%q rt=%q ok=%v", ch, rt, ok)
	}
	// Same handle, new reply target — refresh should update it.
	_, _ = r.Resolve("telegram", "user:telegram:5", "chat-2", nil)
	_, rt, _ = r.LookupUser(res.IdentityID)
	if rt != "chat-2" {
		t.Fatalf("reply_to not refreshed: got %q", rt)
	}
}

func TestLookupUserUnknown(t *testing.T) {
	r := newTestRegistry(t)
	if _, _, ok := r.LookupUser("u_doesnotexist"); ok {
		t.Fatal("unknown id should not resolve")
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.db")
	r1, err := Open(path, testLogger())
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	first, err := r1.Resolve("telegram", "user:telegram:alice", "chat-1", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	_ = r1.Close()

	r2, err := Open(path, testLogger())
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer r2.Close()
	ch, rt, ok := r2.LookupUser(first.IdentityID)
	if !ok || ch != "telegram" || rt != "chat-1" {
		t.Fatalf("after reopen: ch=%q rt=%q ok=%v", ch, rt, ok)
	}
	again, err := r2.Resolve("telegram", "user:telegram:alice", "chat-2", nil)
	if err != nil {
		t.Fatalf("resolve2: %v", err)
	}
	if again.IdentityID != first.IdentityID {
		t.Fatalf("identity changed across reopen: %q vs %q", again.IdentityID, first.IdentityID)
	}
}

// --- profiles and linking ---------------------------------------------------

func TestCreateProfileAndGet(t *testing.T) {
	r := newTestRegistry(t)
	p, err := r.CreateProfile("Oscar", "oscar@example.com")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(p.UserID, "u_") || p.DisplayName != "Oscar" || p.Email != "oscar@example.com" {
		t.Fatalf("created profile wrong: %+v", p)
	}
	got, ok, err := r.Get(p.UserID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.DisplayName != "Oscar" || len(got.Identities) != 0 {
		t.Fatalf("fresh profile should have no identities: %+v", got)
	}
	if _, ok, _ := r.Get("u_nope"); ok {
		t.Fatal("unknown profile must not resolve")
	}
}

// Linking is what gives a handle a personal scope — and it must take effect
// immediately, including for a handle already resolved once (its resolution is
// cached, so linking has to evict it).
func TestLinkGivesHandleTheProfileScope(t *testing.T) {
	r := newTestRegistry(t)
	before, err := r.Resolve("telegram", "user:telegram:42", "chat-9", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if before.Registered() {
		t.Fatal("precondition: handle starts unregistered")
	}

	p, _ := r.CreateProfile("Oscar", "")
	linked, err := r.Link(p.UserID, "user:telegram:42")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if linked.UserID != p.UserID {
		t.Fatalf("link returned user %q, want %q", linked.UserID, p.UserID)
	}

	// Same handle, resolved again: now it carries the profile scope.
	after, err := r.Resolve("telegram", "user:telegram:42", "chat-9", nil)
	if err != nil {
		t.Fatalf("resolve after link: %v", err)
	}
	if after.UserID != p.UserID {
		t.Fatalf("linked handle must resolve to the profile: got %q want %q", after.UserID, p.UserID)
	}
	if after.IdentityID != before.IdentityID {
		t.Fatalf("linking must not change the identity: %q vs %q", after.IdentityID, before.IdentityID)
	}

	ids, err := r.Identities(p.UserID)
	if err != nil || len(ids) != 1 || ids[0].NativeFrom != "user:telegram:42" {
		t.Fatalf("profile identities: %+v err=%v", ids, err)
	}
}

func TestLinkRejectsForeignAndUnknownHandles(t *testing.T) {
	r := newTestRegistry(t)
	_, _ = r.Resolve("telegram", "user:telegram:1", "1", nil)
	a, _ := r.CreateProfile("A", "")
	b, _ := r.CreateProfile("B", "")

	if _, err := r.Link(a.UserID, "user:telegram:1"); err != nil {
		t.Fatalf("first link should succeed: %v", err)
	}
	if _, err := r.Link(b.UserID, "user:telegram:1"); err == nil {
		t.Fatal("a handle owned by another profile must not be linkable")
	}
	if _, err := r.Link(a.UserID, "user:telegram:never-seen"); err == nil {
		t.Fatal("linking a handle that never wrote in must fail")
	}
	// Re-linking to the same profile is idempotent.
	if _, err := r.Link(a.UserID, "user:telegram:1"); err != nil {
		t.Fatalf("idempotent link: %v", err)
	}
}

func TestUnlinkReturnsHandleToUnregistered(t *testing.T) {
	r := newTestRegistry(t)
	_, _ = r.Resolve("telegram", "user:telegram:1", "1", nil)
	_, _ = r.Resolve("telegram", "user:telegram:2", "2", nil)
	p, _ := r.CreateProfile("A", "")
	one, err := r.Link(p.UserID, "user:telegram:1")
	if err != nil {
		t.Fatalf("link 1: %v", err)
	}
	if _, err := r.Link(p.UserID, "user:telegram:2"); err != nil {
		t.Fatalf("link 2: %v", err)
	}

	if err := r.Unlink(p.UserID, one.ID); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	back, _ := r.Resolve("telegram", "user:telegram:1", "1", nil)
	if back.Registered() {
		t.Fatalf("unlinked handle must lose its scope, got %q", back.UserID)
	}
	// The remaining handle still works.
	remaining, _ := r.Resolve("telegram", "user:telegram:2", "2", nil)
	if remaining.UserID != p.UserID {
		t.Fatalf("remaining handle lost its scope: %q", remaining.UserID)
	}

	// Stripping the last handle would leave the account unreachable.
	if err := r.Unlink(p.UserID, remaining.IdentityID); err == nil {
		t.Fatal("unlinking the last handle must be refused")
	}
	if err := r.Unlink(p.UserID, "i_nope"); err == nil {
		t.Fatal("unlinking an unknown identity must fail")
	}
}

// Push resolves to a profile's most recently seen *deliverable* handle — a
// webhook identity has no addressable target, so it must never win.
func TestTargetsPreferDeliverableAndRecent(t *testing.T) {
	r := newTestRegistry(t)
	// A telegram handle (deliverable) and a webhook handle (not deliverable).
	_, _ = r.Resolve("telegram", "user:telegram:5", "chat-old", nil)
	_, _ = r.Resolve("webhook", "user:webhook:alice", "req-1", nil)
	p, _ := r.CreateProfile("A", "")
	if _, err := r.Link(p.UserID, "user:telegram:5"); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := r.Link(p.UserID, "user:webhook:alice"); err != nil {
		t.Fatalf("link webhook: %v", err)
	}

	targets, err := r.Targets(p.UserID)
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(targets) != 1 || targets[0].Channel != "telegram" {
		t.Fatalf("only the deliverable handle may be a target: %+v", targets)
	}
	ch, rt, ok := r.LookupUser(p.UserID)
	if !ok || ch != "telegram" || rt != "chat-old" {
		t.Fatalf("lookup: ch=%q rt=%q ok=%v", ch, rt, ok)
	}

	// A newer reply target on the same handle wins.
	_, _ = r.Resolve("telegram", "user:telegram:5", "chat-new", nil)
	if _, rt, _ := r.LookupUser(p.UserID); rt != "chat-new" {
		t.Fatalf("most recent target should win, got %q", rt)
	}

	// A profile with no deliverable handle resolves to nothing.
	q, _ := r.CreateProfile("B", "")
	_, _ = r.Resolve("webhook", "user:webhook:bob", "req-2", nil)
	if _, err := r.Link(q.UserID, "user:webhook:bob"); err != nil {
		t.Fatalf("link b: %v", err)
	}
	if _, _, ok := r.LookupUser(q.UserID); ok {
		t.Fatal("an undeliverable profile must not resolve to a target")
	}
}

func TestListReturnsProfilesWithIdentities(t *testing.T) {
	r := newTestRegistry(t)
	if ps, err := r.List(); err != nil || len(ps) != 0 {
		t.Fatalf("empty registry: %v err=%v", ps, err)
	}
	a, _ := r.CreateProfile("A", "a@example.com")
	_, _ = r.Resolve("telegram", "user:telegram:1", "1", nil)
	if _, err := r.Link(a.UserID, "user:telegram:1"); err != nil {
		t.Fatalf("link: %v", err)
	}
	ps, err := r.List()
	if err != nil || len(ps) != 1 {
		t.Fatalf("list: %+v err=%v", ps, err)
	}
	if len(ps[0].Identities) != 1 || ps[0].Email != "a@example.com" {
		t.Fatalf("listed profile incomplete: %+v", ps[0])
	}
}

func TestUpdateProfile(t *testing.T) {
	r := newTestRegistry(t)
	p, _ := r.CreateProfile("A", "a@example.com")
	name, tokens := "Renamed", int64(5000)
	if err := r.Update(p.UserID, &name, nil, &tokens); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _, _ := r.Get(p.UserID)
	if got.DisplayName != "Renamed" || got.Email != "a@example.com" || got.TokensPerDay != 5000 {
		t.Fatalf("partial update wrong: %+v", got)
	}
	if err := r.Update("u_nope", &name, nil, nil); err == nil {
		t.Fatal("updating an unknown profile must fail")
	}
}

// --- legacy migration -------------------------------------------------------

// A database written by the pre-profile release (a single `users` table) must
// come forward with each row becoming a profile whose id IS the old uuid, so
// every scoped key minted before the upgrade keeps working untouched.
func TestMigrateLegacyUsersTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE users (
			uuid         TEXT PRIMARY KEY,
			native_from  TEXT NOT NULL UNIQUE,
			channel      TEXT NOT NULL,
			reply_to     TEXT NOT NULL DEFAULT '',
			username     TEXT NOT NULL DEFAULT '',
			name         TEXT NOT NULL DEFAULT '',
			first_seen   INTEGER NOT NULL,
			last_seen    INTEGER NOT NULL
		);
		INSERT INTO users VALUES
			('u_legacy1', 'user:telegram:123', 'telegram', 'chat-7', 'oscar', 'Oscar', 100, 200),
			('u_legacy2', 'user:webhook:alice', 'webhook', 'req-9', '', '', 300, 400);
	`); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	r, err := Open(path, testLogger())
	if err != nil {
		t.Fatalf("open after legacy schema: %v", err)
	}
	defer r.Close()

	// The old uuid is now the profile id AND the identity id.
	p, ok, err := r.Get("u_legacy1")
	if err != nil || !ok {
		t.Fatalf("legacy row must become a profile: ok=%v err=%v", ok, err)
	}
	if len(p.Identities) != 1 || p.Identities[0].ID != "u_legacy1" {
		t.Fatalf("legacy profile identities: %+v", p.Identities)
	}
	if p.Identities[0].Username != "oscar" || p.Identities[0].Name != "Oscar" {
		t.Fatalf("profile fields lost in migration: %+v", p.Identities[0])
	}

	// Resolving the same native_from returns the SAME id as before the
	// upgrade — that is what makes pre-existing scoped data keep working.
	res, err := r.Resolve("telegram", "user:telegram:123", "chat-8", nil)
	if err != nil {
		t.Fatalf("resolve migrated: %v", err)
	}
	if res.UserID != "u_legacy1" {
		t.Fatalf("migrated handle must keep its uuid as the scope: got %q", res.UserID)
	}
	if res.IdentityID != "u_legacy1" {
		t.Fatalf("migrated identity id: got %q", res.IdentityID)
	}
	// Delivery target refreshed from the live message.
	if _, rt, ok := r.LookupUser("u_legacy1"); !ok || rt != "chat-8" {
		t.Fatalf("migrated target: rt=%q ok=%v", rt, ok)
	}

	// The legacy table is gone; a second open must not fail (idempotence).
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	r2, err := Open(path, testLogger())
	if err != nil {
		t.Fatalf("second open after migration: %v", err)
	}
	defer r2.Close()
	if _, ok, _ := r2.Get("u_legacy1"); !ok {
		t.Fatal("migration must survive a reopen")
	}
}

// --- sink -------------------------------------------------------------------

type captureSink struct {
	got  []router.Inbound
	mu   sync.Mutex
	done chan struct{}
}

func (c *captureSink) Submit(in router.Inbound) {
	c.mu.Lock()
	c.got = append(c.got, in)
	c.mu.Unlock()
	select {
	case c.done <- struct{}{}:
	default:
	}
}

func newCapture() *captureSink { return &captureSink{done: make(chan struct{}, 4)} }

func inbound(from, channel string) router.Inbound {
	return router.Inbound{
		Channel: channel,
		Agent:   "bot",
		Message: session.Message{
			ID: "m1", Type: "user", From: from, Text: "hi",
			Channel: channel, ReplyTo: "12345",
		},
	}
}

func TestSinkStampsRegisteredScope(t *testing.T) {
	r := newTestRegistry(t)
	r.SetAutoClaim(true)
	cap := newCapture()
	sink := NewSink(cap, r, testLogger())
	sink.Submit(inbound("user:telegram:123", "telegram"))

	if len(cap.got) != 1 {
		t.Fatalf("inner got %d events, want 1", len(cap.got))
	}
	out := cap.got[0]
	scope, _ := out.Message.Payload["user_uuid"].(string)
	if scope == "" {
		t.Fatal("auto-claimed handle should stamp a scope")
	}
	if out.Message.From != "user:"+scope {
		t.Fatalf("From=%q want user:%s", out.Message.From, scope)
	}
	if out.Message.To != "agent:bot" {
		t.Fatalf("To=%q want agent:bot", out.Message.To)
	}
	if reg, _ := out.Message.Payload["registered"].(bool); !reg {
		t.Error("registered flag should be true")
	}
	if id, _ := out.Message.Payload["identity_id"].(string); !strings.HasPrefix(id, "i_") {
		t.Errorf("identity_id missing: %v", out.Message.Payload["identity_id"])
	}
	if out.Message.Payload["native_from"] != "user:telegram:123" {
		t.Fatalf("native_from not stashed: %v", out.Message.Payload["native_from"])
	}
	if out.Message.Channel != "telegram" || out.Message.ReplyTo != "12345" {
		t.Fatalf("reply path clobbered: channel=%q reply_to=%q", out.Message.Channel, out.Message.ReplyTo)
	}
}

// The unregistered case is the security-relevant one: the payload must say so
// explicitly, because the actor treats a present-but-empty user_uuid as "no
// scope" rather than falling back to the sender string.
func TestSinkStampsNoScopeWhenUnregistered(t *testing.T) {
	r := newTestRegistry(t)
	cap := newCapture()
	sink := NewSink(cap, r, testLogger())
	sink.Submit(inbound("user:telegram:123", "telegram"))

	out := cap.got[0]
	scope, present := out.Message.Payload["user_uuid"]
	if !present {
		t.Fatal("user_uuid must be present even when empty, so the actor does not fall back to From")
	}
	if s, _ := scope.(string); s != "" {
		t.Fatalf("unregistered handle must stamp an empty scope, got %q", s)
	}
	if reg, _ := out.Message.Payload["registered"].(bool); reg {
		t.Error("registered flag should be false")
	}
	id, _ := out.Message.Payload["identity_id"].(string)
	if !strings.HasPrefix(id, "i_") || out.Message.From != "user:"+id {
		t.Fatalf("From should carry the stable identity id: from=%q id=%q", out.Message.From, id)
	}
}

// Linking a handle that already resolved must take effect on the next inbound
// (the resolution cache is evicted by Link).
func TestSinkUsesProfileScopeAfterLink(t *testing.T) {
	r := newTestRegistry(t)
	cap := newCapture()
	sink := NewSink(cap, r, testLogger())

	sink.Submit(inbound("user:telegram:123", "telegram")) // caches "unregistered"
	p, _ := r.CreateProfile("Oscar", "")
	if _, err := r.Link(p.UserID, "user:telegram:123"); err != nil {
		t.Fatalf("link: %v", err)
	}
	sink.Submit(inbound("user:telegram:123", "telegram"))

	if len(cap.got) != 2 {
		t.Fatalf("inner got %d events, want 2", len(cap.got))
	}
	scope, _ := cap.got[1].Message.Payload["user_uuid"].(string)
	if scope != p.UserID {
		t.Fatalf("linked handle must stamp the profile after a cache hit was possible: got %q want %q", scope, p.UserID)
	}
}

func TestSinkFailOpenOnRegistryError(t *testing.T) {
	// A closed registry: Resolve will error on the DB hit.
	r := newTestRegistry(t)
	_ = r.Close()
	cap := newCapture()
	sink := NewSink(cap, r, testLogger())

	sink.Submit(inbound("user:telegram:123", "telegram"))
	if len(cap.got) != 1 {
		t.Fatalf("fail-open should still forward, got %d", len(cap.got))
	}
	if cap.got[0].Message.From != "user:telegram:123" {
		t.Fatalf("fail-open changed From to %q", cap.got[0].Message.From)
	}
}
