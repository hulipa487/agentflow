package caps

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"agentflow/internal/core/session"
	"agentflow/internal/drivers/shell"
)

func TestShellHandlers(t *testing.T) {
	tp := newTestShellProvider("docker")
	mgr := shell.NewManager([]shell.ShellProvider{tp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := ShellHandlers(mgr)

	// Inject owner into context.
	ctx := session.WithOwner(context.Background(), "session-1")

	// Spawn.
	op := session.Op{Type: "shell.spawn", Image: "alpine:latest", ShellProvider: "docker"}
	resp, ok := h["shell.spawn"](ctx, op)
	if !ok {
		t.Fatalf("spawn failed: %s", resp)
	}
	var spawnResult map[string]any
	if err := json.Unmarshal([]byte(resp), &spawnResult); err != nil {
		t.Fatal(err)
	}
	handleID, _ := spawnResult["id"].(string)
	if handleID == "" {
		t.Fatal("expected handle id")
	}

	// Exec.
	op = session.Op{Type: "shell.exec", ShellHandle: handleID, Cmd: "echo hello"}
	resp, ok = h["shell.exec"](ctx, op)
	if !ok {
		t.Fatalf("exec failed: %s", resp)
	}
	var execResult struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(resp), &execResult); err != nil {
		t.Fatal(err)
	}
	if execResult.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", execResult.ExitCode)
	}

	// Destroy.
	op = session.Op{Type: "shell.destroy", ShellHandle: handleID}
	resp, ok = h["shell.destroy"](ctx, op)
	if !ok || resp != "true" {
		t.Fatalf("destroy failed: %s", resp)
	}
}

func TestShellHandlerNoOwner(t *testing.T) {
	tp := newTestShellProvider("docker")
	mgr := shell.NewManager([]shell.ShellProvider{tp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := ShellHandlers(mgr)

	// No owner in context.
	_, ok := h["shell.spawn"](context.Background(), session.Op{Type: "shell.spawn", Image: "alpine", ShellProvider: "docker"})
	if ok {
		t.Fatal("expected failure with no owner")
	}
}

// TestShellSpawnPassesShellOpts verifies the ShellOpts escape hatch flows from
// op.ShellOpts through to SpawnOpts.ShellOpts (the seam vultr/docker read).
func TestShellSpawnPassesShellOpts(t *testing.T) {
	tp := newTestShellProvider("docker")
	mgr := shell.NewManager([]shell.ShellProvider{tp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := ShellHandlers(mgr)
	ctx := session.WithOwner(context.Background(), "session-1")

	op := session.Op{
		Type:      "shell.spawn",
		ShellProvider: "docker",
		Image:     "alpine:3.20",
		ShellOpts: map[string]any{"region": "ewr", "plan": "vc2-1c-1gb"},
	}
	resp, ok := h["shell.spawn"](ctx, op)
	if !ok {
		t.Fatalf("spawn failed: %s", resp)
	}
	if tp.lastOpts.ShellOpts["region"] != "ewr" {
		t.Fatalf("ShellOpts.region not plumbed: got %v", tp.lastOpts.ShellOpts["region"])
	}
	if tp.lastOpts.ShellOpts["plan"] != "vc2-1c-1gb" {
		t.Fatalf("ShellOpts.plan not plumbed: got %v", tp.lastOpts.ShellOpts["plan"])
	}
	if tp.lastOpts.Image != "alpine:3.20" {
		t.Fatalf("Image not plumbed: got %q", tp.lastOpts.Image)
	}
}

// testShellProvider is a minimal ShellProvider for testing ShellHandlers.
type testShellProvider struct {
	name        string
	handles     map[string]*shell.Handle
	lastOpts    shell.SpawnOpts
}

func (p *testShellProvider) Name() string { return p.name }

func (p *testShellProvider) Spawn(ctx context.Context, opts shell.SpawnOpts) (*shell.Handle, error) {
	p.lastOpts = opts
	return &shell.Handle{ID: "h-1", State: 1, Image: opts.Image, Meta: map[string]any{}}, nil
}

func (p *testShellProvider) Exec(ctx context.Context, handle *shell.Handle, cmd string) (*shell.ExecResult, error) {
	return &shell.ExecResult{Stdout: "ok: " + cmd, ExitCode: 0}, nil
}

func (p *testShellProvider) Read(ctx context.Context, handle *shell.Handle, path string) ([]byte, error) {
	return []byte("content:" + path), nil
}

func (p *testShellProvider) Write(ctx context.Context, handle *shell.Handle, path string, content []byte) error {
	return nil
}

func (p *testShellProvider) Destroy(ctx context.Context, handle *shell.Handle) error {
	return nil
}

func (p *testShellProvider) Alive(handle *shell.Handle) bool { return false }

func newTestShellProvider(name string) *testShellProvider {
	return &testShellProvider{name: name}
}
