package tools

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/search"
	"agentflow/internal/drivers/shell"
)

func TestRegistryExpose(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r, nil)
	as := r.Expose([]string{"builtin:web_search"}, config.ToolsPolicy{Default: "all"}, false)
	if len(as.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(as.Tools))
	}
	if _, ok := as.ByName["builtin:web_search"]; !ok {
		t.Fatal("builtin:web_search not exposed")
	}
}

// TestExposeDefaultNone: with tools.policy.default: none and no skills, an
// agent exposes zero tools — even though tools are registered. Default all
// (or unset) still exposes everything, and explicit skills filter exactly.
func TestExposeDefaultNone(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r, nil)

	as := r.Expose(nil, config.ToolsPolicy{Default: "none"}, false)
	if len(as.Tools) != 0 {
		t.Fatalf("default none + no skills: expected 0 tools, got %d", len(as.Tools))
	}
	if len(as.ByName) != 0 {
		t.Fatalf("default none + no skills: ByName not empty: %d", len(as.ByName))
	}

	all := len(r.tools)
	for _, pol := range []config.ToolsPolicy{{Default: "all"}, {}} {
		as = r.Expose(nil, pol, false)
		if len(as.Tools) != all {
			t.Fatalf("default %q: expected %d tools, got %d", pol.Default, all, len(as.Tools))
		}
	}

	as = r.Expose([]string{"builtin:web_search"}, config.ToolsPolicy{Default: "none"}, false)
	if len(as.Tools) != 1 {
		t.Fatalf("explicit skills under default none: expected 1 tool, got %d", len(as.Tools))
	}
	if _, ok := as.ByName["builtin:web_search"]; !ok {
		t.Fatal("explicit skill not exposed under default none")
	}
}

// TestJSONOmitsEmptyRequired: an empty `required` in a tool schema must never
// be emitted — it round-trips through Lua as `{}`, invalid JSON Schema.
func TestJSONOmitsEmptyRequired(t *testing.T) {
	spec := ToolSpec{
		Name: "t",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"q": map[string]any{"type": "string"}},
			"required":   []string{},
		},
	}
	fn := spec.JSON()["function"].(map[string]any)
	params := fn["parameters"].(map[string]any)
	if _, has := params["required"]; has {
		t.Fatalf("empty required should be omitted, got %v", params["required"])
	}

	// Non-empty required survives; nested empty required is dropped.
	spec.Parameters["required"] = []string{"q"}
	spec.Parameters["properties"] = map[string]any{
		"sub": map[string]any{"type": "object", "required": []any{}, "properties": map[string]any{}},
	}
	params = spec.JSON()["function"].(map[string]any)["parameters"].(map[string]any)
	if _, has := params["required"]; !has {
		t.Fatal("non-empty required should be kept")
	}
	sub := params["properties"].(map[string]any)["sub"].(map[string]any)
	if _, has := sub["required"]; has {
		t.Fatalf("nested empty required should be omitted, got %v", sub["required"])
	}
}

// TestNormalizeSchemaDropsMangledRequired: a `required` that already crossed
// Lua as an object is dropped (defense in depth at any boundary).
func TestNormalizeSchemaDropsMangledRequired(t *testing.T) {
	in := map[string]any{
		"type":     "object",
		"required": map[string]any{},
		"items":    map[string]any{"type": "string", "required": map[string]any{}},
	}
	out := NormalizeSchema(in)
	if _, has := out["required"]; has {
		t.Fatalf("object-form required should be dropped: %v", out["required"])
	}
	items := out["items"].(map[string]any)
	if _, has := items["required"]; has {
		t.Fatalf("nested object-form required should be dropped: %v", items["required"])
	}
	// The input map is not mutated.
	if _, has := in["required"]; !has {
		t.Fatal("NormalizeSchema must copy, not mutate the input")
	}
}

func TestWebSearchUnavailable(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r, nil)
	as := r.Expose([]string{"builtin:web_search"}, config.ToolsPolicy{}, false)
	res, err := as.Invoke(context.Background(), "builtin:web_search", map[string]any{"query": "foo"})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type %T", res)
	}
	if m["ok"] != false {
		t.Fatalf("expected ok=false, got %v", m["ok"])
	}
	if m["unavailable"] != true {
		t.Fatalf("expected unavailable=true, got %v", m["unavailable"])
	}
}

// fakeEngine counts calls and records the request.
type fakeEngine struct {
	got search.Request
	c   int
}

func (f *fakeEngine) Search(ctx context.Context, req search.Request) (*search.Result, error) {
	f.c++
	f.got = req
	return &search.Result{Query: req.Query, Results: []search.WebResult{{Title: "t", URL: "u"}}}, nil
}

func TestWebSearchEngineDispatch(t *testing.T) {
	fake := &fakeEngine{}
	set := &search.Set{
		Engines: map[string]search.Searcher{"fake": fake},
		Default: "fake",
	}
	r := NewRegistry()
	RegisterBuiltins(r, set)
	as := r.Expose([]string{"builtin:web_search"}, config.ToolsPolicy{}, false)

	// count arrives as float64 on the Lua->JSON path; argCount must accept int too.
	for _, count := range []any{float64(3), 7} {
		res, err := as.Invoke(context.Background(), "builtin:web_search", map[string]any{"query": "q", "count": count})
		if err != nil {
			t.Fatal(err)
		}
		m := res.(map[string]any)
		if m["ok"] != true || m["engine"] != "fake" || m["count"] != 1 {
			t.Fatalf("invoke: %v", m)
		}
		if fake.got.Count == 0 {
			t.Fatalf("count %v not forwarded", count)
		}
	}
}

func TestForbidden(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r, nil)
	as := r.Expose([]string{"builtin:web_search"}, config.ToolsPolicy{Forbidden: []string{"builtin:web_search"}}, false)
	if len(as.Tools) != 0 {
		t.Fatalf("expected no tools, got %d", len(as.Tools))
	}
}

func TestRegisterShellBuiltins(t *testing.T) {
	r := NewRegistry()
	mgr := shell.NewManager([]shell.ShellProvider{&fakeShellProvider{name: "docker"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	RegisterShellBuiltins(r, mgr)
	as := r.Expose([]string{"builtin:fs.read", "builtin:fs.write"}, config.ToolsPolicy{}, false)
	for _, name := range []string{"builtin:fs.read", "builtin:fs.write"} {
		if _, ok := as.ByName[name]; !ok {
			t.Fatalf("expected %s to be exposed", name)
		}
	}
	if !as.ByName["builtin:fs.write"].NeedsConfirm {
		t.Fatal("fs.write should require confirm")
	}
}

func TestFSReadTool(t *testing.T) {
	r := NewRegistry()
	mgr := shell.NewManager([]shell.ShellProvider{&fakeShellProvider{name: "docker"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h, err := mgr.Spawn(context.Background(), "session-1", "docker", shell.SpawnOpts{Image: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	RegisterShellBuiltins(r, mgr)
	as := r.Expose([]string{"builtin:fs.read"}, config.ToolsPolicy{}, false)
	ctx := session.WithOwner(context.Background(), "session-1")
	res, err := as.Invoke(ctx, "builtin:fs.read", map[string]any{"handle_id": h.ID, "path": "/tmp/a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["ok"] != true || m["content"] != "content:/tmp/a.txt" {
		t.Fatalf("unexpected fs.read result: %v", m)
	}
}
