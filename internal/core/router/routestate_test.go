package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/gateway"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
	"agentflow/internal/core/supervisor"
)

// stateRouter is a router whose route state is a real store.
func stateRouter(t *testing.T, path string) (*Router, runtime.Store) {
	t.Helper()
	st := mustStore(t, path)
	r := New("loop-src", "", nil, testLogger(t))
	r.SetStateStore(st)
	return r, st
}

// stateRouterWith builds a router over a real store, with the given route
// source and supervisor — the shape the engine wires.
func stateRouterWith(t *testing.T, src string, sup *supervisor.Supervisor) (*Router, runtime.Store) {
	t.Helper()
	st := mustStore(t, filepath.Join(t.TempDir(), "routes.db"))
	r := New(src, "", sup, testLogger(t))
	r.SetStateStore(st)
	return r, st
}

func mustStore(t *testing.T, path string) runtime.Store {
	t.Helper()
	st, err := runtime.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestRouteStateRoundTrip(t *testing.T) {
	r, _ := stateRouter(t, filepath.Join(t.TempDir(), "routes.db"))
	ctx := context.Background()

	// A key that was never set reads as null rather than failing: a handler
	// should not need pcall to ask a question.
	if got, ok := r.stateGet(ctx, "chat:1"); !ok || got != "null" {
		t.Fatalf("unset key = %q ok=%v, want null", got, ok)
	}
	// A value crosses the bridge as JSON and is stored as it arrived.
	if _, ok := r.stateSet(ctx, "chat:1", json.RawMessage(`{"agent":"a","n":2}`), nil); !ok {
		t.Fatal("set failed")
	}
	if got, ok := r.stateGet(ctx, "chat:1"); !ok || got != `{"agent":"a","n":2}` {
		t.Fatalf("get = %q ok=%v", got, ok)
	}

	// list returns the handler's own keys, without the store's prefix.
	all, ok := r.stateList(ctx, "chat:")
	if !ok {
		t.Fatalf("list failed: %s", all)
	}
	var listed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(all), &listed); err != nil {
		t.Fatalf("list is not JSON: %v", err)
	}
	if len(listed) != 1 || string(listed["chat:1"]) != `{"agent":"a","n":2}` {
		t.Fatalf("list = %v", listed)
	}
	// A prefix that matches nothing is an empty object, not an error.
	if none, ok := r.stateList(ctx, "other:"); !ok || none != "{}" {
		t.Fatalf("empty list = %q ok=%v", none, ok)
	}

	if _, ok := r.stateDelete(ctx, "chat:1"); !ok {
		t.Fatal("delete failed")
	}
	if got, _ := r.stateGet(ctx, "chat:1"); got != "null" {
		t.Fatalf("deleted key still reads: %q", got)
	}
	// Deleting what is already gone is not an error: the handler asked for the
	// key to be gone, and it is.
	if _, ok := r.stateDelete(ctx, "chat:1"); !ok {
		t.Fatal("second delete failed")
	}
}

// The property the feature exists for: two instances, two processes, two Lua
// states, one answer — because the memory is in the store, not in either of
// them. It is also what makes a restart forget nothing.
func TestRouteStateIsSharedAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.db")
	a, _ := stateRouter(t, path)
	b, _ := stateRouter(t, path)
	ctx := context.Background()

	if _, ok := a.stateSet(ctx, "chat:9", json.RawMessage(`"picked-a"`), nil); !ok {
		t.Fatal("set on a failed")
	}
	if got, ok := b.stateGet(ctx, "chat:9"); !ok || got != `"picked-a"` {
		t.Fatalf("the other instance does not see the state: %q ok=%v", got, ok)
	}
	// A read-modify-write from the second instance continues from where the
	// first left off — the counter a stateful route handler keeps.
	if got, ok := b.stateList(ctx, "chat:"); !ok || !strings.Contains(got, "picked-a") {
		t.Fatalf("list on b = %q ok=%v", got, ok)
	}
	if _, ok := b.stateDelete(ctx, "chat:9"); !ok {
		t.Fatal("delete on b failed")
	}
	if got, _ := a.stateGet(ctx, "chat:9"); got != "null" {
		t.Fatalf("a still reads a key b deleted: %q", got)
	}
}

// The bounds: a key that could forge a row boundary, a value past the cap, a
// listing too wide for the bridge, and a nil value.
func TestRouteStateBounds(t *testing.T) {
	r, _ := stateRouter(t, filepath.Join(t.TempDir(), "routes.db"))
	ctx := context.Background()

	for _, key := range []string{"", "  ", "bad|key", "with space", "sl/ash", strings.Repeat("x", maxRouteStateKeyLen+1)} {
		if _, ok := r.stateSet(ctx, key, json.RawMessage(`1`), nil); ok {
			t.Errorf("key %q should be refused", key)
		}
		if _, ok := r.stateGet(ctx, key); ok {
			t.Errorf("get with key %q should be refused", key)
		}
	}
	// A value with no value, and one over the cap.
	if _, ok := r.stateSet(ctx, "chat:1", nil, nil); ok {
		t.Error("a set with no value should be refused")
	}
	if _, ok := r.stateSet(ctx, "chat:1", json.RawMessage(`null`), nil); ok {
		t.Error("a null value should be refused; the handler is told to delete instead")
	}
	big := json.RawMessage(`"` + strings.Repeat("x", maxRouteStateValue) + `"`)
	if _, ok := r.stateSet(ctx, "chat:1", big, nil); ok {
		t.Error("a value over the cap should be refused")
	}
	// And the set that was refused left nothing behind.
	if got, _ := r.stateGet(ctx, "chat:1"); got != "null" {
		t.Fatalf("a refused set wrote something: %q", got)
	}
	if _, ok := r.stateSet(ctx, "chat:1", json.RawMessage(`1`), ptr(-1.0)); ok {
		t.Error("a negative lifetime should be refused")
	}
}

// A key is kept while it is in use: the default is a window, an explicit zero
// means keep it until it is deleted, and a lapse reads as unset.
func TestRouteStateLifetime(t *testing.T) {
	r, st := stateRouter(t, filepath.Join(t.TempDir(), "routes.db"))
	ctx := context.Background()

	if _, ok := r.stateSet(ctx, "chat:1", json.RawMessage(`1`), nil); !ok {
		t.Fatal("set failed")
	}
	row, ok, err := st.GetRow(ctx, routeStatePrefix+"chat:1")
	if err != nil || !ok {
		t.Fatalf("row: ok=%v err=%v", ok, err)
	}
	if row.ExpiresAt.IsZero() {
		t.Fatal("the default lifetime must be a window, not forever")
	}
	if d := time.Until(row.ExpiresAt); d < defaultRouteStateTTL-time.Minute || d > defaultRouteStateTTL+time.Minute {
		t.Fatalf("default lifetime is %v, want about %v", d, defaultRouteStateTTL)
	}

	// ttl_seconds = 0 keeps a key until the handler deletes it.
	if _, ok := r.stateSet(ctx, "chat:2", json.RawMessage(`1`), ptr(0.0)); !ok {
		t.Fatal("set with ttl 0 failed")
	}
	if row, _, _ := st.GetRow(ctx, routeStatePrefix+"chat:2"); !row.ExpiresAt.IsZero() {
		t.Fatalf("ttl 0 must not expire, got %v", row.ExpiresAt)
	}

	// A short lifetime lapses, and the store reports the key gone.
	if _, ok := r.stateSet(ctx, "chat:3", json.RawMessage(`1`), ptr(0.05)); !ok {
		t.Fatal("set with a short ttl failed")
	}
	time.Sleep(80 * time.Millisecond)
	if got, ok := r.stateGet(ctx, "chat:3"); !ok || got != "null" {
		t.Fatalf("a lapsed key still reads: %q ok=%v", got, ok)
	}
}

// No store, no route state — and the handler is told, rather than getting nil
// for every key and routing on a lie.
func TestRouteStateWithoutAStore(t *testing.T) {
	r := New("loop-src", "", nil, testLogger(t))
	ctx := context.Background()
	for what, call := range map[string]func() (string, bool){
		"get":    func() (string, bool) { return r.stateGet(ctx, "chat:1") },
		"set":    func() (string, bool) { return r.stateSet(ctx, "chat:1", json.RawMessage(`1`), nil) },
		"delete": func() (string, bool) { return r.stateDelete(ctx, "chat:1") },
		"list":   func() (string, bool) { return r.stateList(ctx, "chat:") },
	} {
		got, ok := call()
		if ok || !strings.Contains(got, "unavailable") {
			t.Fatalf("%s without a store = %q ok=%v", what, got, ok)
		}
	}
}

// End to end through the VM: a handler that remembers which agent a chat was
// last routed to alternates between two of them, and it holds that memory in
// the store rather than in its own Lua state — which is what makes it work on
// whichever instance received the message.
func TestStatefulRouteHandlerAlternatesAgents(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	recorded := make(chan string, 4)
	probe := func(agent string) session.OpHandler {
		return func(ctx context.Context, op session.Op) (string, bool) {
			recorded <- agent + ":" + op.Text
			return "true", true
		}
	}
	loop := `function loop()
  local msg = session.inbox()
  af.op({ type = "probe.record", text = msg.text })
end`
	defs := map[string]*supervisor.AgentDef{
		"one": {
			Info:     &session.Info{Name: "one", HistoryBudget: 100},
			Handlers: map[string]session.OpHandler{"probe.record": probe("one")},
			LoopSrc:  loop,
		},
		"two": {
			Info:     &session.Info{Name: "two", HistoryBudget: 100},
			Handlers: map[string]session.OpHandler{"probe.record": probe("two")},
			LoopSrc:  loop,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := supervisor.New(defs, gateway.NewRegistry(log), pool.New(2), nil, log)
	sup.Start(ctx)

	routeSrc := `
function loop()
  while true do
    local item = session.inbox()
    local msg = item.message
    local n = router.state.get("chat:" .. msg.from) or 0
    router.state.set("chat:" .. msg.from, n + 1)
    local target = "one"
    if n % 2 == 1 then target = "two" end
    af.op({ type = "deliver", agent = target, key = msg.from, message = msg })
  end
end`
	r, st := stateRouterWith(t, routeSrc, sup)
	go r.Run(ctx)

	r.Submit(Inbound{Channel: "webhook", Agent: "one", Message: session.Message{Type: "user", From: "u1", Text: "first"}})
	r.Submit(Inbound{Channel: "webhook", Agent: "one", Message: session.Message{Type: "user", From: "u1", Text: "second"}})

	want := map[string]bool{"one:first": true, "two:second": true}
	for i := 0; i < 2; i++ {
		select {
		case got := <-recorded:
			if !want[got] {
				t.Fatalf("unexpected delivery %q (want one:first and two:second)", got)
			}
			delete(want, got)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for delivery %d; got %v", i+1, want)
		}
	}

	// The count they alternated on is in the store, so the next instance to
	// receive a message for this chat continues the sequence.
	row, ok, err := st.GetRow(ctx, routeStatePrefix+"chat:u1")
	if err != nil || !ok {
		t.Fatalf("the handler's state did not reach the store: ok=%v err=%v", ok, err)
	}
	if row.Value != "2" {
		t.Fatalf("chat:u1 = %s, want 2", row.Value)
	}
}

func ptr[T any](v T) *T { return &v }
