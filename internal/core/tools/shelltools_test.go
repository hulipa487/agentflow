package tools

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/shell"
)

// fakeShellProvider is the package's one shell fake: it records spawns,
// execs, writes, and destroys, and answers reads with deterministic content.
type fakeShellProvider struct {
	name string

	mu       sync.Mutex
	spawns   int
	execs    []string
	writes   []string
	destroys int
	lastOpts shell.SpawnOpts
}

func (p *fakeShellProvider) Name() string { return p.name }
func (p *fakeShellProvider) Spawn(ctx context.Context, opts shell.SpawnOpts) (*shell.Handle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spawns++
	p.lastOpts = opts
	return &shell.Handle{ID: fmt.Sprintf("h-%d", p.spawns), State: shell.HandleRunning, Image: opts.Image}, nil
}
func (p *fakeShellProvider) Exec(ctx context.Context, h *shell.Handle, cmd string) (*shell.ExecResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.execs = append(p.execs, cmd)
	return &shell.ExecResult{Stdout: "ran: " + cmd, ExitCode: 0, Duration: 7}, nil
}
func (p *fakeShellProvider) Read(ctx context.Context, h *shell.Handle, path string) ([]byte, error) {
	return []byte("content:" + path), nil
}
func (p *fakeShellProvider) Write(ctx context.Context, h *shell.Handle, path string, content []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writes = append(p.writes, path+"="+string(content))
	return nil
}
func (p *fakeShellProvider) Destroy(ctx context.Context, h *shell.Handle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroys++
	h.State = shell.HandleDestroyed
	return nil
}
func (p *fakeShellProvider) Alive(h *shell.Handle) bool { return true }

func shellToolSetup(t *testing.T) (*Registry, *fakeShellProvider) {
	t.Helper()
	p := &fakeShellProvider{name: "docker"}
	mgr := shell.NewManager([]shell.ShellProvider{p}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := NewRegistry()
	RegisterShellBuiltins(r, mgr)
	return r, p
}

// boundShellCtx carries an owner and a bound docker shell profile, as
// execBlocking stamps them for a session of an agent with `shell:` set.
func boundShellCtx() context.Context {
	return session.WithShell(session.WithOwner(context.Background(), "main|s1"), map[string]any{
		"provider": "docker",
		"image":    "alpine:3.20",
		"workdir":  "/work",
	})
}

// TestShellExecLazySpawnAndReuse: builtin:shell.exec resolves the bound profile
// from the op context, lazily spawns on first use, and reuses the handle after.
// The confirm gate fires through AgentSet.Invoke; the confirmed re-dispatch
// calls the spec's Invoke directly.
func TestShellExecLazySpawnAndReuse(t *testing.T) {
	r, p := shellToolSetup(t)
	as := r.Expose(nil, config.ToolsPolicy{}, false)
	ctx := boundShellCtx()

	res, err := as.Invoke(ctx, "builtin:shell.exec", map[string]any{"command": "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if m := res.(map[string]any); m["needs_confirm"] != true {
		t.Fatalf("exec should require confirm by default, got %v", m)
	}
	if p.spawns != 0 {
		t.Fatal("confirm gate must not spawn")
	}

	spec := as.ByName["builtin:shell.exec"]
	res, err = spec.Invoke(ctx, map[string]any{"command": "ls"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["ok"] != true || m["stdout"] != "ran: ls" || m["exit_code"] != 0 || m["duration_ms"] != int64(7) || m["handle_id"] != "h-1" {
		t.Fatalf("unexpected exec result: %v", m)
	}
	if p.spawns != 1 {
		t.Fatalf("expected 1 spawn, got %d", p.spawns)
	}
	if p.lastOpts.Image != "alpine:3.20" || p.lastOpts.WorkDir != "/work" {
		t.Fatalf("spawn opts not from the bound profile: %+v", p.lastOpts)
	}

	res, err = spec.Invoke(ctx, map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if m := res.(map[string]any); m["handle_id"] != "h-1" {
		t.Fatalf("handle not reused: %v", m)
	}
	if p.spawns != 1 || len(p.execs) != 2 {
		t.Fatalf("expected reuse (1 spawn, 2 execs), got %d spawns, %d execs", p.spawns, len(p.execs))
	}
}

// TestShellToolsNoProfileUnavailable: an agent without a bound shell profile
// gets honest-unavailable from all three tools — never a raw error, never a
// spawn.
func TestShellToolsNoProfileUnavailable(t *testing.T) {
	r, p := shellToolSetup(t)
	as := r.Expose(nil, config.ToolsPolicy{}, false)
	ctx := session.WithOwner(context.Background(), "main|s1") // no shell in ctx

	for _, name := range []string{"builtin:shell.exec", "builtin:shell.write", "builtin:shell.destroy"} {
		res, err := as.ByName[name].Invoke(ctx, map[string]any{"command": "ls", "path": "/x", "content": "y"})
		if err != nil {
			t.Fatalf("%s: raw error leaked: %v", name, err)
		}
		m := res.(map[string]any)
		if m["ok"] != false || m["unavailable"] != true {
			t.Fatalf("%s: expected honest-unavailable, got %v", name, m)
		}
	}
	if p.spawns != 0 {
		t.Fatal("unavailable path must not spawn")
	}
}

// TestShellWriteAndDestroyTools: write goes through the lazily-spawned handle;
// destroy tears it down (idempotently) and the next exec spawns a fresh one.
func TestShellWriteAndDestroyTools(t *testing.T) {
	r, p := shellToolSetup(t)
	as := r.Expose(nil, config.ToolsPolicy{}, false)
	ctx := boundShellCtx()

	res, err := as.ByName["builtin:shell.write"].Invoke(ctx, map[string]any{"path": "/tmp/a.txt", "content": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if m := res.(map[string]any); m["ok"] != true || m["handle_id"] != "h-1" {
		t.Fatalf("unexpected write result: %v", m)
	}
	if len(p.writes) != 1 || p.writes[0] != "/tmp/a.txt=hi" {
		t.Fatalf("write not recorded: %v", p.writes)
	}

	destroy := as.ByName["builtin:shell.destroy"]
	res, err = destroy.Invoke(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := res.(map[string]any); m["ok"] != true || m["destroyed"] != true {
		t.Fatalf("unexpected destroy result: %v", m)
	}
	res, err = destroy.Invoke(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := res.(map[string]any); m["ok"] != true || m["destroyed"] != false {
		t.Fatalf("second destroy should be a no-op, got %v", m)
	}
	if p.destroys != 1 {
		t.Fatalf("expected 1 destroy, got %d", p.destroys)
	}

	res, err = as.ByName["builtin:shell.exec"].Invoke(ctx, map[string]any{"command": "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if m := res.(map[string]any); m["handle_id"] != "h-2" {
		t.Fatalf("exec after destroy should spawn a fresh handle, got %v", m)
	}
	if p.spawns != 2 {
		t.Fatalf("expected 2 spawns, got %d", p.spawns)
	}
}

// TestShellToolsCapabilityGateContract: main.go policy-forbids the shell tools
// for agents without the shell.exec capability — this locks the names and the
// Expose behavior that gate relies on.
func TestShellToolsCapabilityGateContract(t *testing.T) {
	r, _ := shellToolSetup(t)
	as := r.Expose(nil, config.ToolsPolicy{Forbidden: []string{
		"builtin:shell.exec", "builtin:shell.write", "builtin:shell.destroy",
	}}, false)
	for _, name := range []string{"builtin:shell.exec", "builtin:shell.write", "builtin:shell.destroy"} {
		if _, ok := as.ByName[name]; ok {
			t.Fatalf("%s must be forbidden for agents without shell.exec", name)
		}
	}
	if _, ok := as.ByName["builtin:fs.read"]; !ok {
		t.Fatal("unrelated shell tools must stay exposed")
	}
}
