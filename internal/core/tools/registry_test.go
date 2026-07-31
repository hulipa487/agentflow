package tools

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/shell"
)

func TestRegistryExpose(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r)
	as := r.Expose([]string{"builtin:web_search"}, config.ToolsPolicy{Default: "all"}, false)
	if len(as.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(as.Tools))
	}
	if _, ok := as.ByName["builtin:web_search"]; !ok {
		t.Fatal("builtin:web_search not exposed")
	}
}

func TestWebSearchUnavailable(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r)
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

func TestForbidden(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r)
	as := r.Expose([]string{"builtin:web_search"}, config.ToolsPolicy{Forbidden: []string{"builtin:web_search"}}, false)
	if len(as.Tools) != 0 {
		t.Fatalf("expected no tools, got %d", len(as.Tools))
	}
}

func TestRegisterShellBuiltins(t *testing.T) {
	r := NewRegistry()
	mgr := shell.NewManager([]shell.ShellProvider{&testShellProvider{name: "docker"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	RegisterShellBuiltins(r, mgr)
	as := r.Expose([]string{"builtin:fs.read", "builtin:fs.write", "builtin:git.status", "builtin:git.commit"}, config.ToolsPolicy{}, false)
	for _, name := range []string{"builtin:fs.read", "builtin:fs.write", "builtin:git.status", "builtin:git.commit"} {
		if _, ok := as.ByName[name]; !ok {
			t.Fatalf("expected %s to be exposed", name)
		}
	}
	if !as.ByName["builtin:fs.write"].NeedsConfirm {
		t.Fatal("fs.write should require confirm")
	}
	if !as.ByName["builtin:git.commit"].NeedsConfirm {
		t.Fatal("git.commit should require confirm")
	}
}

func TestFSReadTool(t *testing.T) {
	r := NewRegistry()
	mgr := shell.NewManager([]shell.ShellProvider{&testShellProvider{name: "docker"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

type testShellProvider struct{ name string }

func (p *testShellProvider) Name() string { return p.name }
func (p *testShellProvider) Spawn(ctx context.Context, opts shell.SpawnOpts) (*shell.Handle, error) {
	return &shell.Handle{ID: "h-1", State: shell.HandleRunning, Image: opts.Image}, nil
}
func (p *testShellProvider) Exec(ctx context.Context, handle *shell.Handle, cmd string) (*shell.ExecResult, error) {
	return &shell.ExecResult{Stdout: "ran: " + cmd, ExitCode: 0}, nil
}
func (p *testShellProvider) Read(ctx context.Context, handle *shell.Handle, path string) ([]byte, error) {
	return []byte("content:" + path), nil
}
func (p *testShellProvider) Write(ctx context.Context, handle *shell.Handle, path string, content []byte) error { return nil }
func (p *testShellProvider) Destroy(ctx context.Context, handle *shell.Handle) error { return nil }
func (p *testShellProvider) Alive(handle *shell.Handle) bool { return true }
