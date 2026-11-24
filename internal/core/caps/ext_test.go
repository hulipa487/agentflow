package caps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/execguard"
	"agentflow/internal/core/ext"
	"agentflow/internal/core/hosthttp"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/session"
)

func TestAPIHandlersAndGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"path":"` + r.URL.Path + `"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	dir := t.TempDir()
	entry := filepath.Join(dir, "index.ts")
	src := `
import type { ExtHost } from "./af";
export function echo(args: { text: string }, _host: ExtHost) {
  return { echoed: args.text };
}
export function fetch_path(args: { url: string }, host: ExtHost) {
  const resp = host.http.request({ url: args.url });
  return JSON.parse(resp.body);
}
`
	if err := os.WriteFile(entry, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := ext.NewRegistry(ext.HostDeps{
		HTTP: hosthttp.New(discardLogger(), nil, netguard.Policy{AllowPrivate: true}),
		Log:  discardLogger(),
	})
	defer reg.Close(context.Background())

	err := reg.Register(ext.Manifest{
		Name:    "demo",
		Version: "1.0.0",
		Runtime: ext.RuntimeScript,
		Entry:   "./index.ts",
		Capabilities: ext.Capabilities{
			HTTP: &ext.HTTPCapability{AllowedHosts: []string{u.Hostname()}},
		},
		Actions: map[string]ext.ActionSpec{
			"echo":       {Description: "Echo input", ExposeAsTool: true},
			"fetch_path": {Description: "Fetch host path"},
		},
	}, dir)
	if err != nil {
		t.Fatal(err)
	}

	raw := APIHandlers(reg)
	gated := Gate(raw, "apis", "writer", map[string]bool{"apis": true})

	resp, ok := gated["api.list"](context.Background(), session.Op{Type: "api.list"})
	if !ok || !strings.Contains(resp, `"demo"`) {
		t.Fatalf("api.list = (%v, %s)", ok, resp)
	}

	resp, ok = gated["api.call"](context.Background(), session.Op{
		Type:   "api.call",
		API:    "demo",
		Action: "echo",
		Args:   map[string]any{"text": "hello"},
	})
	if !ok {
		t.Fatalf("api.call failed: %s", resp)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resp), &out); err != nil || out["echoed"] != "hello" {
		t.Fatalf("api.call result = %s (%v)", resp, err)
	}

	resp, ok = gated["api.batch"](context.Background(), session.Op{
		Type: "api.batch",
		APICalls: []session.APICallSpec{
			{API: "demo", Action: "echo", Args: map[string]any{"text": "a"}},
			{API: "demo", Action: "fetch_path", Args: map[string]any{"url": srv.URL + "/v2"}},
		},
	})
	if !ok {
		t.Fatalf("api.batch failed: %s", resp)
	}
	var batch []ext.BatchResult
	if err := json.Unmarshal([]byte(resp), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch) != 2 || !batch[0].OK || !batch[1].OK {
		t.Fatalf("batch = %+v", batch)
	}

	// Gate denies all api.* ops when agent lacks "apis".
	denied := Gate(raw, "apis", "writer", map[string]bool{"llm.chat": true})
	if msg, ok := denied["api.call"](context.Background(), session.Op{API: "demo", Action: "echo"}); ok || !strings.Contains(msg, "lacks the apis capability") {
		t.Fatalf("expected apis capability denial, got ok=%v msg=%s", ok, msg)
	}
}

func TestAPIHandlersEnforceBoundaryPolicy(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "index.ts")
	src := `
import type { ExtHost } from "./af";
let counter = 0;
export function read_ok(args: { text: string }, _host: ExtHost) {
  return { ok: true, text: args.text };
}
export function admin_write(args: { amount: number }, _host: ExtHost) {
  counter++;
  return { ok: true, count: counter, amount: args.amount };
}
export function forbidden_act(_args: any, _host: ExtHost) {
  return { ok: true };
}
export function unclassified_act(_args: any, _host: ExtHost) {
  return { ok: true };
}
`
	if err := os.WriteFile(entry, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := ext.NewRegistry(ext.HostDeps{Log: discardLogger()})
	defer reg.Close(context.Background())

	err := reg.Register(ext.Manifest{
		Name:    "vault",
		Version: "1.0.0",
		Runtime: ext.RuntimeScript,
		Entry:   "./index.ts",
		Actions: map[string]ext.ActionSpec{
			"read_ok":          {Kind: "read"},
			"admin_write":      {Kind: "write", RequiresCapability: "vault.admin", NeedsConfirm: true},
			"forbidden_act":    {Kind: "read"},
			"unclassified_act": {},
		},
	}, dir)
	if err != nil {
		t.Fatal(err)
	}

	guard := execguard.NewStore(nil)
	handlers := APIHandlersWithWiring(reg, APIWiring{
		Guard:       guard,
		HostedMode:  true,
		GrantedCaps: map[string]bool{"apis": true}, // deliberately missing "vault.admin"
		ToolsPolicy: config.ToolsPolicy{Forbidden: []string{"api:vault.forbidden_act"}},
		Agent:       "worker",
	})
	baseCtx := session.WithSessionKey(context.Background(), "main|api1")
	baseCtx = session.WithTurnMessage(baseCtx, &session.Message{ID: "m1", Type: "user", From: "u1", Channel: "studio", ReplyTo: "r1"})

	// Allowed read action works.
	if resp, ok := handlers["api.call"](baseCtx, session.Op{API: "vault", Action: "read_ok", Args: map[string]any{"text": "hi"}}); !ok || !strings.Contains(resp, `"hi"`) {
		t.Fatalf("expected allowed read_ok, got ok=%v resp=%s", ok, resp)
	}
	// Missing action-specific capability denied.
	if resp, ok := handlers["api.call"](baseCtx, session.Op{API: "vault", Action: "admin_write", Args: map[string]any{"amount": 5}}); ok || !strings.Contains(resp, "vault.admin") {
		t.Fatalf("expected vault.admin capability denial, got ok=%v resp=%s", ok, resp)
	}
	// Forbidden tool policy denied on direct api.call.
	if resp, ok := handlers["api.call"](baseCtx, session.Op{API: "vault", Action: "forbidden_act"}); ok || !strings.Contains(resp, "forbidden") {
		t.Fatalf("expected policy forbidden denial, got ok=%v resp=%s", ok, resp)
	}
	// Hosted mode denies unclassified actions on direct api.call.
	if resp, ok := handlers["api.call"](baseCtx, session.Op{API: "vault", Action: "unclassified_act"}); ok || !strings.Contains(resp, "unclassified") {
		t.Fatalf("expected hosted unclassified denial, got ok=%v resp=%s", ok, resp)
	}
	// Undeclared action rejected before reaching runner.
	if resp, ok := handlers["api.call"](baseCtx, session.Op{API: "vault", Action: "missing"}); ok || !strings.Contains(resp, "unknown action") {
		t.Fatalf("expected unknown action error, got ok=%v resp=%s", ok, resp)
	}

	// Grant vault.admin -> triggers NeedsConfirm and idempotency checks on both api.call & api.batch.
	adminHandlers := APIHandlersWithWiring(reg, APIWiring{
		Guard:       guard,
		HostedMode:  true,
		GrantedCaps: map[string]bool{"apis": true, "vault.admin": true},
		ToolsPolicy: config.ToolsPolicy{Forbidden: []string{"api:vault.forbidden_act"}},
		Agent:       "worker",
	})
	resp, ok := adminHandlers["api.call"](baseCtx, session.Op{
		API:            "vault",
		Action:         "admin_write",
		Args:           map[string]any{"amount": 42, "token": "secret"},
		IdempotencyKey: "batch-or-single-1",
	})
	if !ok {
		t.Fatalf("expected needs_confirm envelope, got err=%s", resp)
	}
	var nc map[string]any
	if err := json.Unmarshal([]byte(resp), &nc); err != nil {
		t.Fatal(err)
	}
	if nc["needs_confirm"] != true {
		t.Fatalf("expected needs_confirm=true, got %v", nc)
	}
	approvalID, _ := nc["approval_id"].(string)
	if _, err := guard.Decide(baseCtx, approvalID, true, "studio:operator", "studio"); err != nil {
		t.Fatal(err)
	}
	resp, ok = adminHandlers["api.call"](execguard.WithApprovedID(baseCtx, approvalID), session.Op{
		API:            "vault",
		Action:         "admin_write",
		Args:           map[string]any{"amount": 42, "token": "secret"},
		IdempotencyKey: "batch-or-single-1",
	})
	if !ok || !strings.Contains(resp, `"count":1`) {
		t.Fatalf("expected approved execution count=1, got ok=%v resp=%s", ok, resp)
	}

	// api.batch evaluates every item independently: read_ok succeeds, admin_write
	// (needs_confirm) and forbidden_act fail per-item, and missing fails.
	bResp, bOK := adminHandlers["api.batch"](baseCtx, session.Op{
		Type: "api.batch",
		APICalls: []session.APICallSpec{
			{API: "vault", Action: "read_ok", Args: map[string]any{"text": "item0"}},
			{API: "vault", Action: "admin_write", Args: map[string]any{"amount": 9}},
			{API: "vault", Action: "forbidden_act"},
			{API: "vault", Action: "missing"},
		},
	})
	if !bOK {
		t.Fatalf("api.batch failed: %s", bResp)
	}
	var batch []ext.BatchResult
	if err := json.Unmarshal([]byte(bResp), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch) != 4 || !batch[0].OK {
		t.Fatalf("expected item 0 OK, got %+v", batch)
	}
	if batch[1].OK || !strings.Contains(batch[1].Error, "approval required") {
		t.Fatalf("expected item 1 approval required, got %+v", batch[1])
	}
	if batch[2].OK || !strings.Contains(batch[2].Error, "forbidden") {
		t.Fatalf("expected item 2 forbidden, got %+v", batch[2])
	}
	if batch[3].OK || !strings.Contains(batch[3].Error, "unknown action") {
		t.Fatalf("expected item 3 unknown action, got %+v", batch[3])
	}
}
