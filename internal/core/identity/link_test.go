package identity

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- user API tokens --------------------------------------------------------

func TestTokenIssueResolveRevoke(t *testing.T) {
	r := newTestRegistry(t)
	p, _ := r.CreateProfile("A", "")

	tok, err := r.IssueToken(p.UserID)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !strings.HasPrefix(tok, "afu_") {
		t.Fatalf("token shape: %q", tok)
	}
	got, ok := r.TokenUser(tok)
	if !ok || got != p.UserID {
		t.Fatalf("token must resolve to its profile: got %q ok=%v", got, ok)
	}
	for _, bad := range []string{"", "afu_deadbeef", tok[1:], "u" + tok} {
		if _, ok := r.TokenUser(bad); ok {
			t.Errorf("token %q must not resolve", bad)
		}
	}

	if err := r.RevokeToken(p.UserID, tok); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := r.TokenUser(tok); ok {
		t.Fatal("a revoked token must not resolve")
	}

	// A profile cannot revoke another profile's token by presenting its value.
	live, _ := r.IssueToken(p.UserID)
	other, _ := r.CreateProfile("B", "")
	if err := r.RevokeToken(other.UserID, live); err == nil {
		t.Fatal("cross-profile revoke must fail")
	}
	if _, ok := r.TokenUser(live); !ok {
		t.Fatal("the token should still be live")
	}
	if err := r.RevokeToken(p.UserID, "afu_notatoken"); err == nil {
		t.Fatal("revoking an unknown token must fail")
	}
}

// Only the hash is stored: a copy of the database is not a copy of the keys.
func TestTokenIsStoredHashed(t *testing.T) {
	r := newTestRegistry(t)
	p, _ := r.CreateProfile("A", "")
	tok, _ := r.IssueToken(p.UserID)

	var n int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM user_tokens WHERE token_hash = ?`, tok).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Fatal("token is stored in plaintext")
	}
	refs, err := r.Tokens(p.UserID)
	if err != nil {
		t.Fatalf("tokens: %v", err)
	}
	if len(refs) != 1 || refs[0].Fingerprint == "" || refs[0].Fingerprint == tok {
		t.Fatalf("token metadata wrong: %+v", refs)
	}
}

// --- link challenges --------------------------------------------------------

func TestStartLinkAndConsume(t *testing.T) {
	r := newTestRegistry(t)
	_, _ = r.Resolve("telegram", "user:telegram:1", "chat-1", nil)
	p, _ := r.CreateProfile("A", "")

	// An asserted channel cannot be linked: nothing there proves possession.
	if _, _, err := r.StartLink(p.UserID, "webhook", time.Minute); err == nil {
		t.Fatal("an unlinkable channel must refuse to start a link")
	}
	if _, _, err := r.StartLink("u_nope", "telegram", time.Minute); err == nil {
		t.Fatal("an unknown profile must not start a link")
	}

	code, expires, err := r.StartLink(p.UserID, "telegram", time.Minute)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if expires <= time.Now().Unix() {
		t.Fatalf("expiry must be in the future: %d", expires)
	}
	if _, err := r.ConsumeLink("telegram", "user:telegram:1", "NOSUCHCD"); err == nil {
		t.Fatal("an unknown code must not link")
	}
	if _, err := r.ConsumeLink("webhook", "user:telegram:1", code); err == nil {
		t.Fatal("a code issued for telegram must not link a webhook handle")
	}

	userID, err := r.ConsumeLink("telegram", "user:telegram:1", code)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if userID != p.UserID {
		t.Fatalf("linked to %q, want %q", userID, p.UserID)
	}
	// Single use.
	if _, err := r.ConsumeLink("telegram", "user:telegram:1", code); err == nil {
		t.Fatal("a link code must not be reusable")
	}
	// And the handle now carries the profile scope.
	res, _ := r.Resolve("telegram", "user:telegram:1", "chat-1", nil)
	if res.UserID != p.UserID {
		t.Fatalf("linked handle scope: got %q want %q", res.UserID, p.UserID)
	}
}

func TestLinkCodeExpiresAndNeedsAKnownHandle(t *testing.T) {
	r := newTestRegistry(t)
	_, _ = r.Resolve("telegram", "user:telegram:2", "chat-2", nil)
	p, _ := r.CreateProfile("A", "")

	code, _, err := r.StartLink(p.UserID, "telegram", time.Minute)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Age the challenge past its expiry.
	if _, err := r.db.Exec(`UPDATE link_challenges SET expires_at = ? WHERE code_hash = ?`,
		time.Now().Add(-time.Minute).Unix(), hashSecret(code)); err != nil {
		t.Fatalf("age: %v", err)
	}
	if _, err := r.ConsumeLink("telegram", "user:telegram:2", code); err == nil {
		t.Fatal("an expired code must not link")
	}

	// A handle that has never written in has no identity to link.
	fresh, _, err := r.StartLink(p.UserID, "telegram", time.Minute)
	if err != nil {
		t.Fatalf("start 2: %v", err)
	}
	if _, err := r.ConsumeLink("telegram", "user:telegram:never", fresh); err == nil {
		t.Fatal("a handle with no identity must not be linkable")
	}
}

// Codes are read aloud and retyped, so they are case-insensitive.
func TestLinkCodeIsCaseInsensitive(t *testing.T) {
	r := newTestRegistry(t)
	_, _ = r.Resolve("telegram", "user:telegram:3", "chat-3", nil)
	p, _ := r.CreateProfile("A", "")
	code, _, _ := r.StartLink(p.UserID, "telegram", time.Minute)
	if _, err := r.ConsumeLink("telegram", "user:telegram:3", strings.ToLower(code)); err != nil {
		t.Fatalf("lowercase code should work: %v", err)
	}
}

// --- invites ----------------------------------------------------------------

func TestInviteIssueAndRedeem(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.RedeemInvite(""); err == nil {
		t.Fatal("an empty invite must be rejected")
	}
	if err := r.RedeemInvite("NOSUCHIN"); err == nil {
		t.Fatal("an unknown invite must be rejected")
	}
	code, err := r.IssueInvite(time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := r.RedeemInvite(code); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if err := r.RedeemInvite(code); err == nil {
		t.Fatal("an invite must be single-use")
	}
	// Case-insensitive, like link codes.
	second, _ := r.IssueInvite(time.Hour)
	if err := r.RedeemInvite(strings.ToLower(second)); err != nil {
		t.Fatalf("lowercase invite: %v", err)
	}
}

func TestExpiredInviteIsRejectedAndPruned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.db")
	r, err := Open(path, testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	code, err := r.IssueInvite(time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := r.db.Exec(`UPDATE invites SET expires_at = ? WHERE code_hash = ?`,
		time.Now().Add(-time.Minute).Unix(), hashSecret(code)); err != nil {
		t.Fatalf("age: %v", err)
	}
	if err := r.RedeemInvite(code); err == nil {
		t.Fatal("an expired invite must be rejected")
	}
	// Boot prunes expired, unredeemed invites.
	_ = r.Close()
	r2, err := Open(path, testLogger())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()
	var n int
	if err := r2.db.QueryRow(`SELECT COUNT(*) FROM invites`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expired invites should be pruned at boot, found %d", n)
	}
}

// --- sink: in-channel link confirmation -------------------------------------

func TestSinkCompletesLinkAndSwallowsTheMessage(t *testing.T) {
	r := newTestRegistry(t)
	cap := newCapture()
	sink := NewSink(cap, r, testLogger())
	var replied []string
	sink.SetReply(func(channel, replyTo, text string) {
		replied = append(replied, text)
	})

	p, _ := r.CreateProfile("A", "")
	code, _, err := r.StartLink(p.UserID, "telegram", time.Minute)
	if err != nil {
		t.Fatalf("start link: %v", err)
	}

	in := inbound("user:telegram:123", "telegram")
	in.Message.Text = "/link " + code
	sink.Submit(in)

	if len(cap.got) != 0 {
		t.Fatalf("a link command must never reach a loop, got %d forwarded", len(cap.got))
	}
	if len(replied) != 1 || !strings.Contains(replied[0], "Linked") {
		t.Fatalf("expected a confirmation reply, got %v", replied)
	}
	got, ok, _ := r.Get(p.UserID)
	if !ok || len(got.Identities) != 1 {
		t.Fatalf("profile should own the handle: %+v", got.Identities)
	}
}

func TestSinkAnswersLinkFailuresInChannel(t *testing.T) {
	r := newTestRegistry(t)
	cap := newCapture()
	sink := NewSink(cap, r, testLogger())
	var replied []string
	sink.SetReply(func(channel, replyTo, text string) {
		replied = append(replied, text)
	})

	// A bare "/link" gets guidance rather than being passed to a loop.
	bare := inbound("user:telegram:123", "telegram")
	bare.Message.Text = "/link"
	sink.Submit(bare)

	// A wrong code gets the reason.
	wrong := inbound("user:telegram:123", "telegram")
	wrong.Message.Text = "/link NOSUCHCD"
	sink.Submit(wrong)

	if len(cap.got) != 0 {
		t.Fatalf("link commands must be swallowed, forwarded %d", len(cap.got))
	}
	if len(replied) != 2 {
		t.Fatalf("expected two replies, got %v", replied)
	}
	if !strings.Contains(replied[0], "/v1/users/me/links") {
		t.Errorf("bare /link should explain where codes come from: %q", replied[0])
	}
	if !strings.Contains(replied[1], "failed") {
		t.Errorf("a bad code should report failure: %q", replied[1])
	}
}

// A message that merely mentions /link is chat, not a command — and an
// unlinkable channel's traffic is never interpreted as one.
func TestSinkLeavesNonCommandsAlone(t *testing.T) {
	r := newTestRegistry(t)
	cap := newCapture()
	sink := NewSink(cap, r, testLogger())
	sink.SetReply(func(string, string, string) {})

	for _, text := range []string{
		"please /link this later",
		"/link two words",
		"/linkfoo",
	} {
		in := inbound("user:telegram:123", "telegram")
		in.Message.Text = text
		sink.Submit(in)
	}
	if len(cap.got) != 3 {
		t.Fatalf("expected 3 forwarded messages, got %d", len(cap.got))
	}

	// webhook is asserted, so its body is never treated as a command.
	cap2 := newCapture()
	sink2 := NewSink(cap2, r, testLogger())
	wh := inbound("user:webhook:alice", "webhook")
	wh.Message.Text = "/link ABCDEFGH"
	sink2.Submit(wh)
	if len(cap2.got) != 1 {
		t.Fatal("an unlinkable channel must pass through untouched")
	}
}
