package caps

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
)

// stateHarness is the ops over a real store, with one session's context.
func stateHarness(t *testing.T) (map[string]session.OpHandler, context.Context, context.Context) {
	t.Helper()
	st, err := runtime.OpenSQLite(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := SessionStateHandlers{Store: st}.Handlers()
	mine := session.WithSessionKey(context.Background(), "bot|chat-1")
	other := session.WithSessionKey(context.Background(), "bot|chat-2")
	return h, mine, other
}

func TestSessionStateRoundTrip(t *testing.T) {
	h, ctx, other := stateHarness(t)

	res, ok := h["session.state.set"](ctx, session.Op{Key: "cursor", Value: float64(3)})
	if !ok {
		t.Fatalf("set failed: %s", res)
	}
	got, ok := h["session.state.get"](ctx, session.Op{Key: "cursor"})
	if !ok || got != "3" {
		t.Fatalf("get = %q ok=%v, want the stored number", got, ok)
	}

	// A table round-trips as a table: the op carries the caller's Lua value and
	// storage is its JSON form, which is what get hands back.
	if _, ok := h["session.state.set"](ctx, session.Op{Key: "plan", Value: map[string]any{"step": "two", "n": float64(2)}}); !ok {
		t.Fatal("set of a table failed")
	}
	got, ok = h["session.state.get"](ctx, session.Op{Key: "plan"})
	if !ok {
		t.Fatal("get of a table failed")
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(got), &plan); err != nil {
		t.Fatalf("stored table is not JSON: %v", err)
	}
	if plan["step"] != "two" || plan["n"] != float64(2) {
		t.Fatalf("table did not round-trip: %v", plan)
	}

	// A key that was never set reads as null rather than failing: a loop should
	// not need pcall to ask a question.
	got, ok = h["session.state.get"](ctx, session.Op{Key: "never"})
	if !ok || got != "null" {
		t.Fatalf("unset key = %q ok=%v, want null", got, ok)
	}

	// list returns what this session holds.
	all, ok := h["session.state.list"](ctx, session.Op{})
	if !ok {
		t.Fatalf("list failed: %s", all)
	}
	var listed map[string]any
	if err := json.Unmarshal([]byte(all), &listed); err != nil {
		t.Fatalf("list is not JSON: %v", err)
	}
	if len(listed) != 2 || listed["cursor"] != float64(3) {
		t.Fatalf("list = %v", listed)
	}

	// delete removes one entry.
	if _, ok := h["session.state.delete"](ctx, session.Op{Key: "cursor"}); !ok {
		t.Fatal("delete failed")
	}
	if got, _ := h["session.state.get"](ctx, session.Op{Key: "cursor"}); got != "null" {
		t.Fatalf("deleted key still reads: %q", got)
	}

	// A different session sees none of it, and its own writes stay its own.
	if got, ok := h["session.state.get"](other, session.Op{Key: "plan"}); !ok || got != "null" {
		t.Fatalf("another session read this one's state: %q", got)
	}
	if _, ok := h["session.state.set"](other, session.Op{Key: "plan", Value: "mine"}); !ok {
		t.Fatal("set on the other session failed")
	}
	if got, _ := h["session.state.get"](ctx, session.Op{Key: "plan"}); got == `"mine"` {
		t.Fatal("sessions share state")
	}
}

// The key is validated because it is composed into a store key: a "|" would let
// a key forge the boundary and reach another session's rows.
func TestSessionStateKeyIsValidated(t *testing.T) {
	h, ctx, _ := stateHarness(t)
	for _, key := range []string{"", "  ", "bad|key", "with space", "sl/ash", strings.Repeat("x", 129)} {
		if _, ok := h["session.state.set"](ctx, session.Op{Key: key, Value: "v"}); ok {
			t.Errorf("key %q should be refused", key)
		}
		if _, ok := h["session.state.get"](ctx, session.Op{Key: key}); ok {
			t.Errorf("get with key %q should be refused", key)
		}
	}
	// A session-less context is refused too: state belongs to a session.
	if _, ok := h["session.state.set"](context.Background(), session.Op{Key: "k", Value: "v"}); ok {
		t.Error("an op with no session in context should be refused")
	}
}

// A loop with a bug must not be able to grow the shared store without limit.
func TestSessionStateIsBounded(t *testing.T) {
	h, ctx, _ := stateHarness(t)

	if _, ok := h["session.state.set"](ctx, session.Op{Key: "big", Value: strings.Repeat("x", maxStateValue+1)}); ok {
		t.Fatal("a value over the cap should be refused")
	}
	if _, ok := h["session.state.set"](ctx, session.Op{Key: "empty"}); ok {
		t.Fatal("a set with no value should be refused")
	}
	first := ""
	for i := 0; i < maxStateKeys; i++ {
		key := "kx" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if i == 0 {
			first = key
		}
		if _, ok := h["session.state.set"](ctx, session.Op{Key: key, Value: "v"}); !ok {
			t.Fatalf("set %d failed before the cap", i)
		}
	}
	// One more distinct key is refused...
	if _, ok := h["session.state.set"](ctx, session.Op{Key: "one-too-many", Value: "v"}); ok {
		t.Fatalf("the %dth key should be refused", maxStateKeys+1)
	}
	// ...but updating a key the session already holds still works.
	if _, ok := h["session.state.set"](ctx, session.Op{Key: first, Value: "updated"}); !ok {
		t.Fatal("a full session must still be able to update what it holds")
	}
}
