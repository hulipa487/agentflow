package shell

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// testProvider is a ShellProvider that replaces real docker with in-memory
// command recording — no Docker host needed for tests.
type testProvider struct {
	name      string
	handles   map[string]*Handle
	spawned   []SpawnOpts
	execCmds  []string
	destroyed []string
}

func newTestProvider(name string) *testProvider {
	return &testProvider{name: name, handles: map[string]*Handle{}}
}

func (p *testProvider) Name() string { return p.name }

func (p *testProvider) Spawn(ctx context.Context, opts SpawnOpts) (*Handle, error) {
	p.spawned = append(p.spawned, opts)
	h := &Handle{ID: "h-" + p.name + "-1", State: HandleRunning, Image: opts.Image, internal: "test-container-id"}
	p.handles[h.ID] = h
	return h, nil
}

func (p *testProvider) Exec(ctx context.Context, handle *Handle, cmd string) (*ExecResult, error) {
	p.execCmds = append(p.execCmds, cmd)
	return &ExecResult{Stdout: "ok: " + cmd, ExitCode: 0}, nil
}

func (p *testProvider) Read(ctx context.Context, handle *Handle, path string) ([]byte, error) {
	return []byte("content:" + path), nil
}

func (p *testProvider) Write(ctx context.Context, handle *Handle, path string, content []byte) error {
	return nil
}

func (p *testProvider) Destroy(ctx context.Context, handle *Handle) error {
	p.destroyed = append(p.destroyed, handle.ID)
	handle.State = HandleDestroyed
	return nil
}

func (p *testProvider) Alive(handle *Handle) bool {
	return handle.State == HandleRunning
}

func TestManagerSpawnExecReap(t *testing.T) {
	tp := newTestProvider("docker")
	mgr := NewManager([]ShellProvider{tp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	h, err := mgr.Spawn(ctx, "session-1", "docker", SpawnOpts{Image: "alpine:latest", Network: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if h.State != HandleRunning {
		t.Fatal("expected running state")
	}

	res, err := mgr.Exec(ctx, "session-1", h.ID, "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "echo hello") {
		t.Fatalf("unexpected stdout: %s", res.Stdout)
	}

	readContent, err := mgr.Read(ctx, "session-1", h.ID, "/tmp/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(readContent) != "content:/tmp/file.txt" {
		t.Fatalf("unexpected read content: %q", readContent)
	}

	// Reap should destroy the handle.
	mgr.ReapSession("session-1")
	if len(tp.destroyed) != 1 {
		t.Fatalf("expected 1 destroy, got %d", len(tp.destroyed))
	}
}

func TestManagerOwnershipEnforcement(t *testing.T) {
	tp := newTestProvider("docker")
	mgr := NewManager([]ShellProvider{tp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	h, _ := mgr.Spawn(ctx, "alice", "docker", SpawnOpts{Image: "alpine"})

	// bob cannot exec alice's handle.
	_, err := mgr.Exec(ctx, "bob", h.ID, "echo hi")
	if err == nil {
		t.Fatal("expected ownership error, got nil")
	}

	// alice can.
	_, err = mgr.Exec(ctx, "alice", h.ID, "echo hi")
	if err != nil {
		t.Fatal(err)
	}
}

func TestManagerUnknownProvider(t *testing.T) {
	mgr := NewManager([]ShellProvider{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := mgr.Spawn(context.Background(), "s", "nonexistent", SpawnOpts{})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

// TestDockerPersistsStateAcrossCalls proves the defining property of the
// persistent Docker driver: filesystem and environment state survive across
// Exec calls within one handle. A file written (and an env var set at Spawn)
// in one call is visible in the next. Skipped when no docker daemon is
// reachable (e.g. CI without Docker Desktop).
func TestDockerPersistsStateAcrossCalls(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker daemon not reachable; skipping persistent-shell integration test")
	}
	p := NewDockerProvider(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	h, err := p.Spawn(ctx, SpawnOpts{
		Image: "alpine:3.20",
		Env:   map[string]string{"MSG": "persisted"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer func() { _ = p.Destroy(ctx, h) }()

	// Write a file using a spawn-time env var, in one Exec.
	if _, err := p.Exec(ctx, h, "echo $MSG > /tmp/once && echo wrote"); err != nil {
		t.Fatalf("exec write: %v", err)
	}

	// A subsequent Exec must see /tmp/once and the env var — same container.
	res, err := p.Exec(ctx, h, "test -f /tmp/once && cat /tmp/once")
	if err != nil {
		t.Fatalf("exec check: %v", err)
	}
	if !strings.Contains(res.Stdout, "persisted") {
		t.Fatalf("persistence violated: /tmp/once not preserved across calls (stdout=%q)", res.Stdout)
	}
}

// dockerAvailable reports whether the docker CLI can reach a daemon. Used only
// to gate the one-shot integration test; cheap to call once per test run.
func dockerAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	return cmd.Run() == nil
}
