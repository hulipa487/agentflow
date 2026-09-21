package identity

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// postgresDSN points the shared-store cases at a server:
//
//	AGENTFLOW_TEST_POSTGRES='postgres://user:pass@localhost:5432/agentflow?sslmode=disable' \
//	  go test ./internal/core/identity/ ./internal/core/credentials/
//
// Unset, they skip — SQLite is the default everywhere, and the suite has to run
// on a machine with nothing installed. Every case tags its rows with a per-run
// id, so running against a server that already holds data is safe, and running
// twice in a row is safe.
var postgresDSN = os.Getenv("AGENTFLOW_TEST_POSTGRES")

func pgTag(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "i" + hex.EncodeToString(b) + ":"
}

// The fleet properties, on the backend a fleet runs on — and the only place the
// PostgreSQL statements execute at all. The schema is created one statement at a
// time, "?" is bound as "$n", and every statement here has to mean what it means
// in a local file: a handle is one identity, a link holds everywhere, and a
// single-use code is single-use. Each block below covers a distinct group of
// statements, so a syntax or type difference cannot hide in an untested corner.
func TestPostgresSharedIdentity(t *testing.T) {
	if postgresDSN == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
	}
	a, b := openAt(t, postgresDSN), openAt(t, postgresDSN)
	id := pgTag(t)

	// --- identities: mint, adopt, refresh -----------------------------------
	handle := id + "tg:1"
	first, err := a.Resolve("telegram", handle, "chat-a", map[string]any{"username": "oscar"})
	if err != nil {
		t.Fatalf("resolve on a: %v", err)
	}
	second, err := b.Resolve("telegram", handle, "chat-b", nil)
	if err != nil {
		t.Fatalf("resolve on b: %v", err)
	}
	if first.IdentityID != second.IdentityID {
		t.Fatalf("one handle, two identities: %q vs %q", first.IdentityID, second.IdentityID)
	}
	stored, ok, err := a.lookupIdentity(handle)
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if stored.ReplyTo != "chat-b" {
		t.Fatalf("the delivery target should be the most recent one, got %q", stored.ReplyTo)
	}
	if stored.Username != "oscar" {
		t.Fatalf("profile fields did not round-trip: %+v", stored)
	}

	// --- profiles and links -------------------------------------------------
	p, err := a.CreateProfile("Oscar", "oscar@example.com")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if _, err := a.Link(p.UserID, handle); err != nil {
		t.Fatalf("link: %v", err)
	}
	linked, err := b.Resolve("telegram", handle, "chat-b", nil)
	if err != nil {
		t.Fatalf("resolve after link: %v", err)
	}
	if linked.UserID != p.UserID {
		t.Fatalf("a link on one instance must hold on another: %q want %q", linked.UserID, p.UserID)
	}
	if got, ok, err := b.Get(p.UserID); err != nil || !ok || got.Email != "oscar@example.com" {
		t.Fatalf("profile read on b: %+v ok=%v err=%v", got, ok, err)
	}
	if err := b.Update(p.UserID, nil, nil, ptr(int64(5000))); err != nil {
		t.Fatalf("update: %v", err)
	}
	if n, err := a.LimitFor(p.UserID); err != nil || n != 5000 {
		t.Fatalf("limit after update on b: %d err=%v", n, err)
	}

	// --- user API tokens ----------------------------------------------------
	tok, err := a.IssueToken(p.UserID)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	if owner, ok := b.TokenUser(tok); !ok || owner != p.UserID {
		t.Fatalf("a token issued on one instance must resolve on another: owner=%q ok=%v", owner, ok)
	}
	refs, err := b.Tokens(p.UserID)
	if err != nil || len(refs) == 0 {
		t.Fatalf("token listing: %+v err=%v", refs, err)
	}
	if len(refs[0].Fingerprint) != 8 {
		t.Fatalf("fingerprint should be the hash prefix, got %q", refs[0].Fingerprint)
	}
	if err := b.RevokeToken(p.UserID, tok); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := a.TokenUser(tok); ok {
		t.Fatal("a revoked token must not resolve anywhere")
	}

	// --- channel linking: a hashed, single-use challenge --------------------
	code, _, err := b.StartLink(p.UserID, "telegram", 0)
	if err != nil {
		t.Fatalf("start link: %v", err)
	}
	handle2 := id + "tg:2"
	if _, err := a.Resolve("telegram", handle2, "chat", nil); err != nil {
		t.Fatalf("resolve second handle: %v", err)
	}
	if _, err := a.ConsumeLink("telegram", handle2, strings.ToLower(code)); err != nil {
		t.Fatalf("consume link: %v", err)
	}
	if _, err := b.ConsumeLink("telegram", handle2, code); err == nil {
		t.Fatal("a consumed link code must not be replayable, on any instance")
	}

	// --- invites: the same compare-and-set, for registration ----------------
	invite, err := a.IssueInvite(0)
	if err != nil {
		t.Fatalf("issue invite: %v", err)
	}
	if err := b.RedeemInvite(invite); err != nil {
		t.Fatalf("redeem on b: %v", err)
	}
	if err := a.RedeemInvite(invite); err == nil {
		t.Fatal("an invite must be single-use across instances")
	}
}

// Channel traits rewrite stored rows, and a fleet has to agree on what its
// channels vouch for whatever instance asks.
func TestPostgresTraitsApplyToStoredRows(t *testing.T) {
	if postgresDSN == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
	}
	a, b := openAt(t, postgresDSN), openAt(t, postgresDSN)
	channel := pgTag(t) + "chan"
	handle := pgTag(t) + "tg:3"

	if _, err := a.Resolve(channel, handle, "chat", nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := b.SetChannelTraits(map[string]ChannelTraits{channel: Traits("telegram")}); err != nil {
		t.Fatalf("set traits: %v", err)
	}
	res, err := a.Resolve(channel, handle, "chat", nil)
	if err != nil {
		t.Fatalf("resolve after traits: %v", err)
	}
	if !res.Linkable || res.Trust != TrustVerified {
		t.Fatalf("traits must apply to an existing row: %+v", res)
	}
}

// A profile provisioned from a login on one instance is the account the same
// login resolves to on every other.
func TestPostgresProvisionedLoginIsShared(t *testing.T) {
	if postgresDSN == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES is unset; skipping the postgres backend")
	}
	a, b := openAt(t, postgresDSN), openAt(t, postgresDSN)
	key := "https://issuer.example|" + pgTag(t)

	id, created, err := a.ProvisionOIDC(key, "Oscar", "oscar@example.com", true)
	if err != nil || !created {
		t.Fatalf("provision: created=%v err=%v", created, err)
	}
	again, created, err := b.ProvisionOIDC(key, "Oscar", "oscar@example.com", true)
	if err != nil {
		t.Fatalf("provision on b: %v", err)
	}
	if created {
		t.Fatal("a second instance must not create a second account for one login")
	}
	if again.ID != id.ID || again.UserID != id.UserID {
		t.Fatalf("the same login resolved to different accounts: %+v vs %+v", again, id)
	}
	// JIT off, and no account yet: a refusal, not an error.
	if _, _, err := b.ProvisionOIDC("https://issuer.example|"+pgTag(t)+"other", "", "", false); err == nil {
		t.Fatal("an unprovisioned login must be refused when jit is off")
	}
}

func ptr[T any](v T) *T { return &v }
