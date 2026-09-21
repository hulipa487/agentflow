package webui

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/accounting"
	"agentflow/internal/core/identity"
	"agentflow/internal/core/runtime"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// userConsole builds a console over a real profile store and ledger, with the
// file store left out (its absence must degrade, not fail).
func userConsole(t *testing.T) (*httptest.Server, *identity.Registry, runtime.Store, string) {
	t.Helper()
	dir := t.TempDir()
	reg, err := identity.Open(filepath.Join(dir, "identity.db"), quiet())
	if err != nil {
		t.Fatalf("open identity: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	store, err := runtime.OpenSQLite(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	p, err := reg.CreateProfile("Oscar", "oscar@example.com")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if _, err := reg.Resolve("telegram", "user:telegram:1", "chat-1", nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := reg.Link(p.UserID, "user:telegram:1"); err != nil {
		t.Fatalf("link: %v", err)
	}

	ui := New(Deps{Users: UserDeps{
		Identities: reg,
		Store:      store,
		Events:     store,
		Journal:    store,
		Quota:      accounting.New(store, reg.LimitFor, 0),
	}})
	srv := httptest.NewServer(ui.API())
	t.Cleanup(srv.Close)
	return srv, reg, store, p.UserID
}

func get(t *testing.T, srv *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
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

func post(t *testing.T, srv *httptest.Server, path, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
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

func TestConsoleUsersListAndDetail(t *testing.T) {
	srv, _, store, userID := userConsole(t)
	day := runtime.DayKey(time.Now())
	if err := store.RecordUsage(runtime.UsageRecord{
		UserID: userID, Agent: "bot", Model: "m", Kind: "chat",
		Input: 400, Output: 60, Cached: 256, OK: true, At: time.Now(),
	}, false); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	if err := store.RecordMessage(t.Context(), runtime.JournalEntry{
		ID: "m1", Ts: time.Now().Unix(), Direction: "in", Status: "routed",
		Channel: "telegram", Sender: "user:" + userID, UserUUID: userID, Text: "hello",
	}); err != nil {
		t.Fatalf("record journal: %v", err)
	}

	// The list carries the day, the usage and the quota standing.
	status, list := get(t, srv, "/admin/api/users")
	if status != http.StatusOK {
		t.Fatalf("list: status %d body %v", status, list)
	}
	if list["day"] != day {
		t.Errorf("day = %v, want %q", list["day"], day)
	}
	users, _ := list["users"].([]any)
	if len(users) != 1 {
		t.Fatalf("expected one user: %v", list["users"])
	}
	row := users[0].(map[string]any)
	if row["user_id"] != userID || row["display_name"] != "Oscar" || row["identities"] != float64(1) {
		t.Fatalf("row: %v", row)
	}
	usage, _ := row["usage_today"].(map[string]any)
	if usage["input"] != float64(400) || usage["cached"] != float64(256) {
		t.Fatalf("row usage: %v", row["usage_today"])
	}

	// The detail carries the handles, the audit tail and the usage.
	status, detail := get(t, srv, "/admin/api/users/"+userID)
	if status != http.StatusOK {
		t.Fatalf("detail: status %d body %v", status, detail)
	}
	profile, _ := detail["profile"].(map[string]any)
	if profile["email"] != "oscar@example.com" {
		t.Fatalf("profile: %v", detail["profile"])
	}
	ids, _ := profile["identities"].([]any)
	if len(ids) != 1 || ids[0].(map[string]any)["channel"] != "telegram" {
		t.Fatalf("identities: %v", profile["identities"])
	}
	audit, _ := detail["audit"].([]any)
	if len(audit) != 1 || audit[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("audit tail: %v", detail["audit"])
	}
	if _, ok := detail["usage_today"]; !ok {
		t.Fatalf("detail should carry usage: %v", detail)
	}

	// An unknown profile is a 404, not an empty object.
	if status, out := get(t, srv, "/admin/api/users/u_nope"); status != http.StatusNotFound {
		t.Fatalf("unknown user: status %d body %v", status, out)
	}
}

func TestConsoleUserLimitAndUnlink(t *testing.T) {
	srv, reg, _, userID := userConsole(t)

	status, out := post(t, srv, "/admin/api/users/"+userID+"/limit", `{"tokens_per_day":2500}`)
	if status != http.StatusOK {
		t.Fatalf("set limit: status %d body %v", status, out)
	}
	p, _, _ := reg.Get(userID)
	if p.TokensPerDay != 2500 {
		t.Fatalf("limit not persisted: %d", p.TokensPerDay)
	}
	// Zero means inherit; negative is nonsense.
	if status, out := post(t, srv, "/admin/api/users/"+userID+"/limit", `{"tokens_per_day":0}`); status != http.StatusOK {
		t.Fatalf("clearing the limit should work: %d %v", status, out)
	}
	if status, _ := post(t, srv, "/admin/api/users/"+userID+"/limit", `{"tokens_per_day":-5}`); status != http.StatusBadRequest {
		t.Fatalf("a negative limit must be refused: %d", status)
	}

	// Unlinking the last handle would leave the account unreachable.
	ids, _ := reg.Identities(userID)
	status, out = post(t, srv, "/admin/api/users/"+userID+"/unlink", `{"identity_id":"`+ids[0].ID+`"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("unlinking the last handle must be refused: %d %v", status, out)
	}
	// A malformed body is a 400 rather than a panic.
	if status, _ := post(t, srv, "/admin/api/users/"+userID+"/unlink", `{`); status != http.StatusBadRequest {
		t.Fatalf("malformed body: %d", status)
	}
	if status, _ := post(t, srv, "/admin/api/users/"+userID+"/unlink", `{}`); status != http.StatusBadRequest {
		t.Fatalf("missing identity_id: %d", status)
	}
}

func TestConsoleInvites(t *testing.T) {
	srv, reg, _, _ := userConsole(t)
	status, out := post(t, srv, "/admin/api/users/invites", `{}`)
	if status != http.StatusOK {
		t.Fatalf("invite: status %d body %v", status, out)
	}
	code, _ := out["invite"].(string)
	if code == "" {
		t.Fatalf("no invite code returned: %v", out)
	}
	// The code is real: it redeems exactly once.
	if err := reg.RedeemInvite(code); err != nil {
		t.Fatalf("issued invite should redeem: %v", err)
	}
	if err := reg.RedeemInvite(code); err == nil {
		t.Fatal("an invite must be single-use")
	}
}

// With no identity layer the surface says why, instead of answering as if there
// were no users.
func TestConsoleUsersWithoutIdentityLayer(t *testing.T) {
	srv := httptest.NewServer(New(Deps{}).API())
	defer srv.Close()

	status, out := get(t, srv, "/admin/api/users")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status %d body %v", status, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "identity") {
		t.Fatalf("the refusal should name the missing layer: %v", out)
	}
	if status, _ := post(t, srv, "/admin/api/users/invites", `{}`); status != http.StatusServiceUnavailable {
		t.Fatalf("invites without identity: %d", status)
	}
}
