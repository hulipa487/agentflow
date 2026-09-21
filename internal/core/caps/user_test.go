package caps

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/accounting"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
)

// userFixture wires the user surface with fakes for everything that lives
// outside caps: the projection comes from a map, credentials from a set.
func userFixture(t *testing.T, limit int64) (map[string]session.OpHandler, runtime.Store, string) {
	t.Helper()
	dir := t.TempDir()
	ledger, err := runtime.OpenSQLite(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { _ = ledger.Close() })

	profiles := map[string]LoopProfile{
		"u_1": {
			UserID: "u_1", DisplayName: "Oscar",
			Identities: []LoopIdentity{{Channel: "telegram", Username: "oscar", Name: "Oscar"}},
		},
	}
	creds := map[string]bool{"u_1/openai": true}
	quota := accounting.New(ledger, func(userID string) (int64, error) {
		if limit <= 0 {
			return 0, nil
		}
		return limit, nil
	}, 0)

	h := UserHandlers{
		Profile: func(userID string) (LoopProfile, bool, error) {
			p, ok := profiles[userID]
			return p, ok, nil
		},
		Profiles: func() ([]LoopProfile, error) {
			out := make([]LoopProfile, 0, len(profiles))
			for _, p := range profiles {
				out = append(out, p)
			}
			return out, nil
		},
		Ledger:        ledger,
		Quota:         quota,
		HasCredential: func(userID, service string) bool { return creds[userID+"/"+service] },
	}.Handlers()
	return h, ledger, "u_1"
}

func ctxForUser(id string) context.Context {
	if id == "" {
		return context.Background()
	}
	return session.WithUserUUID(context.Background(), id)
}

// call runs one op and decodes its object response, failing the test on a
// refusal (tests that expect refusal call the handler directly).
func call(t *testing.T, h map[string]session.OpHandler, name string, ctx context.Context, op session.Op) map[string]any {
	t.Helper()
	resp, ok := h[name](ctx, op)
	if !ok {
		t.Fatalf("%s failed: %s", name, resp)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("%s decode %q: %v", name, resp, err)
	}
	return out
}

// callList runs an op that answers a bare array (user.list).
func callList(t *testing.T, h map[string]session.OpHandler, name string, ctx context.Context, op session.Op) []map[string]any {
	t.Helper()
	resp, ok := h[name](ctx, op)
	if !ok {
		t.Fatalf("%s failed: %s", name, resp)
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("%s decode %q: %v", name, resp, err)
	}
	return out
}

func TestUserCurrent(t *testing.T) {
	h, _, _ := userFixture(t, 0)

	got := call(t, h, "user.current", ctxForUser("u_1"), session.Op{Type: "user.current"})
	if got["registered"] != true || got["id"] != "u_1" || got["display_name"] != "Oscar" {
		t.Fatalf("current: %v", got)
	}
	ids, _ := got["identities"].([]any)
	if len(ids) != 1 || ids[0].(map[string]any)["channel"] != "telegram" {
		t.Fatalf("identities: %v", got["identities"])
	}
	// The projection must not carry an email address: a loop has no business
	// with one.
	if _, present := got["email"]; present {
		t.Fatalf("a loop must not see an email address: %v", got)
	}

	// A turn with no user is a guest, not an error.
	guest := call(t, h, "user.current", ctxForUser(""), session.Op{Type: "user.current"})
	if guest["registered"] != false {
		t.Fatalf("guest should report registered=false: %v", guest)
	}

	// A user id with no profile is an error: a linked handle always has one.
	if resp, ok := h["user.current"](ctxForUser("u_gone"), session.Op{Type: "user.current"}); ok {
		t.Fatalf("a missing profile must fail loudly, got %s", resp)
	}
}

func TestUserUsageReportsTokensAndQuota(t *testing.T) {
	h, ledger, _ := userFixture(t, 1000)
	if err := ledger.RecordUsage(runtime.UsageRecord{
		UserID: "u_1", Agent: "bot", Model: "m", Kind: "chat",
		Input: 300, Output: 50, Cached: 128, Reasoning: 5, OK: true, At: time.Now(),
	}, false); err != nil {
		t.Fatalf("record: %v", err)
	}

	got := call(t, h, "user.usage", ctxForUser("u_1"), session.Op{Type: "user.usage"})
	if got["input"] != float64(300) || got["output"] != float64(50) || got["cached"] != float64(128) {
		t.Fatalf("token classes: %v", got)
	}
	if got["billable"] != float64(350) {
		t.Fatalf("billable should be input+output: %v", got)
	}
	if got["limit"] != float64(1000) || got["remaining"] != float64(650) || got["unlimited"] != false {
		t.Fatalf("quota standing: %v", got)
	}

	// No user: nothing to report, and no error.
	guest := call(t, h, "user.usage", ctxForUser(""), session.Op{Type: "user.usage"})
	if guest["registered"] != false {
		t.Fatalf("guest usage: %v", guest)
	}
}

func TestUserUsageSaysUnlimitedRatherThanInventingBudget(t *testing.T) {
	h, _, _ := userFixture(t, 0) // no limit anywhere
	got := call(t, h, "user.usage", ctxForUser("u_1"), session.Op{Type: "user.usage"})
	if got["unlimited"] != true || got["limit"] != float64(0) {
		t.Fatalf("an unset limit must be reported plainly: %v", got)
	}
	if _, present := got["remaining"]; present {
		t.Fatalf("no remaining budget should be invented: %v", got)
	}
}

func TestUserHasCredentialNeverExposesTheValue(t *testing.T) {
	h, _, _ := userFixture(t, 0)

	got := call(t, h, "user.has_credential", ctxForUser("u_1"),
		session.Op{Type: "user.has_credential", Service: "openai"})
	if got["present"] != true || got["service"] != "openai" {
		t.Fatalf("has_credential: %v", got)
	}
	// The answer carries only existence.
	for _, forbidden := range []string{"value", "secret", "key"} {
		if _, present := got[forbidden]; present {
			t.Fatalf("the credential value must never cross: %v", got)
		}
	}

	missing := call(t, h, "user.has_credential", ctxForUser("u_1"),
		session.Op{Type: "user.has_credential", Service: "stripe"})
	if missing["present"] != false {
		t.Fatalf("unknown service: %v", missing)
	}
	// A userless turn holds nothing.
	guest := call(t, h, "user.has_credential", ctxForUser(""),
		session.Op{Type: "user.has_credential", Service: "openai"})
	if guest["present"] != false {
		t.Fatalf("a guest holds nothing: %v", guest)
	}
	// The service name is required.
	if resp, ok := h["user.has_credential"](ctxForUser("u_1"), session.Op{Type: "user.has_credential"}); ok {
		t.Fatalf("a missing service must fail: %s", resp)
	}
}

// The directory is maintenance-only: a user's own turn cannot read another
// account, and the refusal names what was needed.
func TestUserDirectoryRequiresMaintenanceProvenance(t *testing.T) {
	h, _, _ := userFixture(t, 0)

	user := ctxForUser("u_1")
	for _, opType := range []string{"user.get", "user.list"} {
		resp, ok := h[opType](user, session.Op{Type: opType, UserID: "u_1"})
		if ok {
			t.Fatalf("%s must be refused for a user's turn: %s", opType, resp)
		}
		if !strings.Contains(resp, "maintenance") {
			t.Fatalf("%s refusal should name the requirement: %s", opType, resp)
		}
	}

	// A service context (an agent hop) is not maintenance either.
	if _, ok := h["user.list"](context.Background(), session.Op{Type: "user.list"}); ok {
		t.Fatal("a service context must not read the directory")
	}

	// Engine-fired provenance may.
	maint := session.WithProvenanceKind(context.Background(), "scheduler")
	list := callList(t, h, "user.list", maint, session.Op{Type: "user.list"})
	if len(list) != 1 || list[0]["id"] != "u_1" {
		t.Fatalf("user.list should return the profiles: %v", list)
	}
	one := call(t, h, "user.get", maint, session.Op{Type: "user.get", UserID: "u_1"})
	if one["id"] != "u_1" || one["display_name"] != "Oscar" {
		t.Fatalf("maintenance lookup: %v", one)
	}
	if _, present := one["email"]; present {
		t.Fatalf("the directory must not carry emails: %v", one)
	}

	// A missing profile is reported, not invented.
	if resp, ok := h["user.get"](maint, session.Op{Type: "user.get", UserID: "u_nope"}); ok {
		t.Fatalf("unknown profile must fail: %s", resp)
	}
	if resp, ok := h["user.get"](maint, session.Op{Type: "user.get"}); ok {
		t.Fatalf("a missing user_id must fail: %s", resp)
	}
}

// The surface stays honest when the identity layer is off: no projection means
// no directory and no profile, but usage still reports a guest.
func TestUserSurfaceWithoutIdentityLayer(t *testing.T) {
	ledger, err := runtime.OpenSQLite(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	h := UserHandlers{Ledger: ledger}.Handlers()

	if got := call(t, h, "user.current", ctxForUser("u_1"), session.Op{Type: "user.current"}); got["registered"] != false {
		t.Fatalf("without a projection there is no current user: %v", got)
	}
	maint := session.WithProvenanceKind(context.Background(), "system")
	if resp, ok := h["user.list"](maint, session.Op{Type: "user.list"}); ok {
		t.Fatalf("no registry means no directory: %s", resp)
	}
	if resp, ok := h["user.get"](maint, session.Op{Type: "user.get", UserID: "u_1"}); ok {
		t.Fatalf("no registry means no lookup: %s", resp)
	}

	// A nil ledger is an honest error rather than a panic.
	bare := UserHandlers{Profile: func(string) (LoopProfile, bool, error) {
		return LoopProfile{UserID: "u_1"}, true, nil
	}}.Handlers()
	if resp, ok := bare["user.usage"](ctxForUser("u_1"), session.Op{Type: "user.usage"}); ok {
		t.Fatalf("no ledger means no usage: %s", resp)
	}
}

var errBoom = errors.New("boom")

// A failing projection must surface as a failure, never as a guest.
func TestUserCurrentPropagatesLookupErrors(t *testing.T) {
	h := UserHandlers{Profile: func(string) (LoopProfile, bool, error) {
		return LoopProfile{}, false, errBoom
	}, Ledger: nil}.Handlers()
	resp, ok := h["user.current"](ctxForUser("u_1"), session.Op{Type: "user.current"})
	if ok || !strings.Contains(resp, "boom") {
		t.Fatalf("lookup error should propagate: ok=%v %s", ok, resp)
	}
	_ = slog.New(slog.NewTextHandler(io.Discard, nil))
}
