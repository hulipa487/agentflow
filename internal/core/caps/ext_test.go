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
