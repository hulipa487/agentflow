package users

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/identity"
	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newAPI(t *testing.T, cfg config.UsersConfig) (*httptest.Server, *identity.Registry) {
	t.Helper()
	reg, err := identity.Open(filepath.Join(t.TempDir(), "identity.db"), discardLogger())
	if err != nil {
		t.Fatalf("open identity: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	srv := httptest.NewServer(New(reg, cfg, discardLogger(), Options{}).Handler())
	t.Cleanup(srv.Close)
	return srv, reg
}

// do issues a request and decodes a JSON object response (empty map when the
// body is empty or not an object).
func do(t *testing.T, srv *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func register(t *testing.T, srv *httptest.Server, extra map[string]any) (string, string) {
	t.Helper()
	body := map[string]any{"display_name": "Oscar", "email": "oscar@example.com"}
	for k, v := range extra {
		body[k] = v
	}
	status, out := do(t, srv, "POST", Prefix+"/register", "", body)
	if status != http.StatusCreated {
		t.Fatalf("register: status %d body %v", status, out)
	}
	token, _ := out["token"].(string)
	userID, _ := out["user_id"].(string)
	if token == "" || userID == "" {
		t.Fatalf("register response incomplete: %v", out)
	}
	return userID, token
}

func TestRegisterAndMe(t *testing.T) {
	srv, reg := newAPI(t, config.UsersConfig{Enabled: true})
	userID, token := register(t, srv, nil)

	// The token is the profile's credential.
	if got, ok := reg.TokenUser(token); !ok || got != userID {
		t.Fatalf("token→profile: got %q ok=%v", got, ok)
	}

	status, me := do(t, srv, "GET", Prefix+"/me", token, nil)
	if status != http.StatusOK {
		t.Fatalf("me: status %d body %v", status, me)
	}
	if me["user_id"] != userID || me["display_name"] != "Oscar" || me["email"] != "oscar@example.com" {
		t.Fatalf("me payload: %v", me)
	}
	if ids, ok := me["identities"].([]any); !ok || len(ids) != 0 {
		t.Fatalf("a fresh profile has no identities: %v", me["identities"])
	}
	// Token metadata is listed, but never a token itself.
	toks, _ := me["tokens"].([]any)
	if len(toks) != 1 {
		t.Fatalf("expected one token listed: %v", me["tokens"])
	}
	if fp, _ := toks[0].(map[string]any)["fingerprint"].(string); fp == "" || fp == token {
		t.Fatalf("token fingerprint must be a fingerprint, got %q", fp)
	}

	status, patched := do(t, srv, "PATCH", Prefix+"/me", token, map[string]string{"display_name": "Renamed"})
	if status != http.StatusOK || patched["display_name"] != "Renamed" {
		t.Fatalf("patch: status %d body %v", status, patched)
	}
}

func TestMeRequiresAValidToken(t *testing.T) {
	srv, _ := newAPI(t, config.UsersConfig{Enabled: true})
	for _, tok := range []string{"", "afu_deadbeef", "not-a-token"} {
		status, out := do(t, srv, "GET", Prefix+"/me", tok, nil)
		if status != http.StatusUnauthorized {
			t.Errorf("token %q: status %d (want 401)", tok, status)
		}
		if out["error"] == nil {
			t.Errorf("token %q: expected an error body, got %v", tok, out)
		}
	}
	// A revoked token stops working.
	_, token := register(t, srv, nil)
	if status, _ := do(t, srv, "GET", Prefix+"/me", token, nil); status != http.StatusOK {
		t.Fatalf("live token: %d", status)
	}
}

func TestInviteModeRegistration(t *testing.T) {
	srv, reg := newAPI(t, config.UsersConfig{Enabled: true, Registration: "invite"})

	status, out := do(t, srv, "POST", Prefix+"/register", "", nil)
	if status != http.StatusForbidden || !strings.Contains(errString(out), "invite-only") {
		t.Fatalf("open registration must be refused in invite mode: %d %v", status, out)
	}
	status, out = do(t, srv, "POST", Prefix+"/register", "", map[string]string{"invite": "BADCODE1"})
	if status != http.StatusForbidden {
		t.Fatalf("a bad invite must be refused: %d %v", status, out)
	}

	code, err := reg.IssueInvite(time.Hour)
	if err != nil {
		t.Fatalf("issue invite: %v", err)
	}
	register(t, srv, map[string]any{"invite": code})

	status, _ = do(t, srv, "POST", Prefix+"/register", "", map[string]string{"invite": code})
	if status != http.StatusForbidden {
		t.Fatalf("an invite must be single-use: %d", status)
	}
}

// The full possession proof: register, ask for a challenge, send it from the
// handle, and watch the profile gain that handle.
func TestLinkFlowEndToEnd(t *testing.T) {
	srv, reg := newAPI(t, config.UsersConfig{Enabled: true})
	userID, token := register(t, srv, nil)

	status, link := do(t, srv, "POST", Prefix+"/me/links", token, map[string]string{"channel": "telegram"})
	if status != http.StatusCreated {
		t.Fatalf("start link: status %d body %v", status, link)
	}
	code, _ := link["code"].(string)
	if code == "" {
		t.Fatalf("no code returned: %v", link)
	}
	if ins, _ := link["instructions"].(string); !strings.Contains(ins, code) {
		t.Errorf("instructions should carry the code: %q", ins)
	}

	// Starting a link needs a profile token...
	if status, _ := do(t, srv, "POST", Prefix+"/me/links", "", map[string]string{"channel": "telegram"}); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated link start: %d", status)
	}
	// ...and a channel that can prove possession.
	if status, out := do(t, srv, "POST", Prefix+"/me/links", token, map[string]string{"channel": "webhook"}); status != http.StatusBadRequest {
		t.Errorf("webhook must not be linkable: %d %v", status, out)
	}
	if status, _ := do(t, srv, "POST", Prefix+"/me/links", token, map[string]any{}); status != http.StatusBadRequest {
		t.Errorf("a missing channel must be rejected: %d", status)
	}

	// The user sends the code from the handle: the sink completes the link and
	// the message never reaches a loop.
	cap := &captureSink{}
	sink := identity.NewSink(cap, reg, discardLogger())
	var replied []string
	sink.SetReply(func(channel, replyTo, text string) { replied = append(replied, text) })
	sink.Submit(textInbound("user:telegram:555", "telegram", "/link "+code))

	if len(cap.got) != 0 {
		t.Fatalf("a link command must not be forwarded, got %d", len(cap.got))
	}
	if len(replied) != 1 || !strings.Contains(replied[0], "Linked") {
		t.Fatalf("expected a confirmation, got %v", replied)
	}

	status, me := do(t, srv, "GET", Prefix+"/me", token, nil)
	if status != http.StatusOK {
		t.Fatalf("me after link: %d", status)
	}
	ids, _ := me["identities"].([]any)
	if len(ids) != 1 {
		t.Fatalf("the handle should be linked: %v", me["identities"])
	}
	first, _ := ids[0].(map[string]any)
	if first["channel"] != "telegram" || first["native_from"] != "user:telegram:555" {
		t.Fatalf("linked identity: %v", first)
	}
	// Push now resolves to the linked handle.
	if ch, rt, ok := reg.LookupUser(userID); !ok || ch != "telegram" || rt != "chat-9" {
		t.Fatalf("lookup after link: ch=%q rt=%q ok=%v", ch, rt, ok)
	}
}

func TestUnlinkThroughAPI(t *testing.T) {
	srv, reg := newAPI(t, config.UsersConfig{Enabled: true})
	_, token := register(t, srv, nil)

	cap := &captureSink{}
	sink := identity.NewSink(cap, reg, discardLogger())
	sink.SetReply(func(string, string, string) {})

	linkOne := func(native string) string {
		t.Helper()
		_, link := do(t, srv, "POST", Prefix+"/me/links", token, map[string]string{"channel": "telegram"})
		code, _ := link["code"].(string)
		sink.Submit(textInbound(native, "telegram", "/link "+code))
		_, me := do(t, srv, "GET", Prefix+"/me", token, nil)
		ids, _ := me["identities"].([]any)
		for _, raw := range ids {
			id, _ := raw.(map[string]any)
			if id["native_from"] == native {
				s, _ := id["id"].(string)
				return s
			}
		}
		t.Fatalf("handle %q was not linked: %v", native, ids)
		return ""
	}
	first := linkOne("user:telegram:555")
	linkOne("user:telegram:666")

	status, out := do(t, srv, "DELETE", Prefix+"/me/links/"+first, token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("unlink: status %d body %v", status, out)
	}
	_, me := do(t, srv, "GET", Prefix+"/me", token, nil)
	if ids, _ := me["identities"].([]any); len(ids) != 1 {
		t.Fatalf("expected one identity left: %v", me["identities"])
	}
	// The last handle cannot be removed: that would make the profile
	// unreachable.
	last, _ := me["identities"].([]any)[0].(map[string]any)["id"].(string)
	if status, _ := do(t, srv, "DELETE", Prefix+"/me/links/"+last, token, nil); status != http.StatusBadRequest {
		t.Fatalf("unlinking the last handle: status %d", status)
	}
	// Nor can someone else's handle be unlinked.
	if status, _ := do(t, srv, "DELETE", Prefix+"/me/links/i_someoneelse", token, nil); status != http.StatusBadRequest {
		t.Fatalf("unlinking an unknown identity: status %d", status)
	}
}

func TestRotateToken(t *testing.T) {
	srv, _ := newAPI(t, config.UsersConfig{Enabled: true})
	_, token := register(t, srv, nil)

	status, out := do(t, srv, "POST", Prefix+"/me/token/rotate", token, nil)
	if status != http.StatusOK {
		t.Fatalf("rotate: status %d body %v", status, out)
	}
	fresh, _ := out["token"].(string)
	if fresh == "" || fresh == token {
		t.Fatalf("rotate must return a new token: %v", out)
	}
	if status, _ := do(t, srv, "GET", Prefix+"/me", token, nil); status != http.StatusUnauthorized {
		t.Errorf("the rotated-away token must stop working: %d", status)
	}
	if status, _ := do(t, srv, "GET", Prefix+"/me", fresh, nil); status != http.StatusOK {
		t.Errorf("the fresh token must work: %d", status)
	}
}

// Registration writes a row, so it is the cheapest thing to flood.
func TestRegistrationIsRateLimited(t *testing.T) {
	srv, _ := newAPI(t, config.UsersConfig{Enabled: true})
	for i := 0; i < 25; i++ {
		if status, _ := do(t, srv, "POST", Prefix+"/register", "", nil); status == http.StatusTooManyRequests {
			return // limited, as intended
		}
	}
	t.Fatal("registration must be rate limited")
}

func TestBadRequests(t *testing.T) {
	srv, _ := newAPI(t, config.UsersConfig{Enabled: true})

	// Malformed body.
	req, err := http.NewRequest("POST", srv.URL+Prefix+"/register", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed JSON: status %d (want 400)", resp.StatusCode)
	}

	// Wrong method.
	if status, _ := do(t, srv, "GET", Prefix+"/register", "", nil); status != http.StatusMethodNotAllowed {
		t.Errorf("GET /register: status %d (want 405)", status)
	}

	// An unknown endpoint under the subtree is a 404, not a panic.
	if status, _ := do(t, srv, "GET", Prefix+"/nope", "", nil); status != http.StatusNotFound {
		// /v1/users/nope is not matched by the /me subtree, so mux answers 404.
		t.Errorf("unknown endpoint: status %d (want 404)", status)
	}
}

// --- helpers ----------------------------------------------------------------

func errString(m map[string]any) string {
	s, _ := m["error"].(string)
	return s
}

type captureSink struct{ got []router.Inbound }

func (c *captureSink) Submit(in router.Inbound) { c.got = append(c.got, in) }

func textInbound(from, channel, text string) router.Inbound {
	return router.Inbound{
		Channel: channel,
		Agent:   "bot",
		Message: session.Message{
			ID: "m1", Type: "user", From: from, Text: text,
			Channel: channel, ReplyTo: "chat-9",
		},
	}
}
