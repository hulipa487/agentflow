package caps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
	"agentflow/internal/core/tools"
)

// TestGateGrantsUnchanged: the common path is the map itself, so an agent that
// holds the capability pays nothing for the check.
func TestGateGrantsUnchanged(t *testing.T) {
	hs := map[string]session.OpHandler{
		"http.request": func(context.Context, session.Op) (string, bool) { return "ok", true },
	}
	got := Gate(hs, "net.http", "writer", map[string]bool{"net.http": true})
	out, ok := got["http.request"](context.Background(), session.Op{})
	if !ok || out != "ok" {
		t.Fatalf("a granted op must run unchanged: %q %v", out, ok)
	}
}

// TestGateDeniesEveryOp: a withheld op stays addressable and fails with a
// message naming the op, the agent and the capability. Dropping the handler
// instead would surface in Lua as "attempt to call a nil value", which tells a
// loop author nothing about why.
func TestGateDeniesEveryOp(t *testing.T) {
	hs := map[string]session.OpHandler{
		"http.request": func(context.Context, session.Op) (string, bool) { return "ok", true },
		"os.env":       func(context.Context, session.Op) (string, bool) { return "ok", true },
	}
	got := Gate(hs, "net.http", "writer", map[string]bool{"memory": true})
	if len(got) != len(hs) {
		t.Fatalf("every op must stay addressable: got %d of %d", len(got), len(hs))
	}
	for name, h := range got {
		out, ok := h(context.Background(), session.Op{})
		if ok {
			t.Fatalf("%s must be denied", name)
		}
		var msg string
		if err := json.Unmarshal([]byte(out), &msg); err != nil {
			t.Fatalf("%s: not a JSON string: %q", name, out)
		}
		for _, want := range []string{name, "writer", "net.http"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("%s: %q must name %q", name, msg, want)
			}
		}
	}
}

// TestGateFailsClosedOnNilCapabilities: a nil set means "holds nothing", which
// is the right reading for a security control.
func TestGateFailsClosedOnNilCapabilities(t *testing.T) {
	hs := map[string]session.OpHandler{
		"store.put": func(context.Context, session.Op) (string, bool) { return "ok", true },
	}
	if _, ok := Gate(hs, "memory", "writer", nil)["store.put"](context.Background(), session.Op{}); ok {
		t.Fatal("a nil capability set must deny")
	}
}

// TestGatedOpDeniesInsideALoop is the point of the change, exercised through a
// real Luau loop: an agent whose capabilities do not include net.http must be
// refused, and told why. Before this, the capability was validated at boot and
// then ignored — the loop would have reached the network.
func TestGatedOpDeniesInsideALoop(t *testing.T) {
	handlers := map[string]session.OpHandler{}
	granted := map[string]bool{"memory": true} // deliberately no net.http
	for k, h := range Gate(HTTPHandlers(discardLogger(), nil, permissive), "net.http", "writer", granted) {
		handlers[k] = h
	}

	gw := &schemaGW{}
	a := session.New("main|test",
		session.Identity{SessionID: "main|test", Agent: "writer", Capabilities: granted},
		&session.Info{Name: "writer", HistoryBudget: 100},
		gw, nil, nil, nil, nil, handlers, pool.New(1), discardLogger())
	a.LoopSrc = `
function loop()
  local msg = session.inbox()
  local ok, res = pcall(http.get, "https://example.com")
  if ok then session.send("reached: " .. tostring(res.status)) else session.send("denied: " .. tostring(res)) end
end
`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	a.Mailbox <- session.Message{ID: "m1", Type: "user", From: "u", Text: "go", Channel: "webhook", ReplyTo: "1"}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gw.mu.Lock()
		n := len(gw.sends)
		gw.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	gw.mu.Lock()
	sends := append([]string(nil), gw.sends...)
	gw.mu.Unlock()
	if len(sends) == 0 {
		t.Fatal("the loop produced no reply")
	}
	if strings.Contains(sends[0], "reached") {
		t.Fatalf("a withheld capability must not reach the network: %s", sends[0])
	}
	if !strings.Contains(sends[0], "net.http capability") {
		t.Fatalf("the denial must name the missing capability: %s", sends[0])
	}
}

// TestModelCannotDispatchToAnOp pins the boundary between the two surfaces.
// Ops are Lua-facing only: tools.run resolves names through the exposed tool
// set, so an op name is never dispatchable by the model. The guard against SSRF
// and the capability gate both sit on the Lua path, so if a refactor ever
// registered an op in the tool registry — or routed tools.run around agentSet —
// every agent-facing protection would be reachable in a way it was not designed
// for. This is the test that says so.
func TestModelCannotDispatchToAnOp(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(tools.ToolSpec{
		Name:        "builtin:probe",
		Description: "a registered tool, so the registry is not empty",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		Invoke:      func(context.Context, map[string]any) (any, error) { return map[string]any{"ok": true}, nil },
	})
	agentSet := reg.Expose([]string{"builtin:probe"}, config.ToolsPolicy{}, false)
	handlers := ToolHandlers(agentSet, ToolWiring{})
	run := handlers["tools.run"]
	ctx := session.WithOwner(context.Background(), "writer")

	ops := []string{
		"http.request", "os.env", "llm.chat", "llm.embed",
		"store.put", "store.get", "store.query", "store.delete",
		"shell.spawn", "shell.exec", "shell.write", "shell.destroy",
		"mail.imap.fetch", "mail.smtp.send",
		"credential.get", "runtime.triggers",
		"tools.list", "tools.run",
	}
	for _, op := range ops {
		resp, ok := run(ctx, session.Op{Type: "tools.run", Tool: op, Args: map[string]any{}})
		if ok {
			t.Fatalf("the model must not be able to dispatch the %s op: %s", op, resp)
		}
		if !strings.Contains(resp, "not available") {
			t.Fatalf("%s: expected a not-available refusal, got %s", op, resp)
		}
	}

	// The registered tool still works, so the refusal above is about the name
	// and not about tools.run being broken.
	if _, ok := run(ctx, session.Op{Type: "tools.run", Tool: "builtin:probe", Args: map[string]any{}}); !ok {
		t.Fatal("a registered tool must remain dispatchable")
	}
}
