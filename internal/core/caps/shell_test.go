package caps

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"agentflow/internal/core/session"
	"agentflow/internal/drivers/shell"
)

func TestShellHandlers(t *testing.T) {
	tp := newTestShellProvider("docker")
	mgr := shell.NewManager([]shell.ShellProvider{tp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := ShellHandlers(mgr, nil, "tester")

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
	h := ShellHandlers(mgr, nil, "tester")

	// No owner in context.
	_, ok := h["shell.spawn"](context.Background(), session.Op{Type: "shell.spawn", Image: "alpine", ShellProvider: "docker"})
	if ok {
		t.Fatal("expected failure with no owner")
	}
}

// TestShellSpawnPassesShellOpts verifies the ShellOpts escape hatch flows from
// op.ShellOpts through to SpawnOpts.ShellOpts (the seam providers read).
func TestShellSpawnPassesShellOpts(t *testing.T) {
	tp := newTestShellProvider("docker")
	mgr := shell.NewManager([]shell.ShellProvider{tp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := ShellHandlers(mgr, nil, "tester")
	ctx := session.WithOwner(context.Background(), "session-1")

	op := session.Op{
		Type:          "shell.spawn",
		ShellProvider: "docker",
		Image:         "alpine:3.20",
		ShellOpts:     map[string]any{"region": "ewr", "plan": "vc2-1c-1gb"},
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
// events records the call order (spawn/write/destroy) so materialization
// tests can assert what happened before the handle reported ready.
type testShellProvider struct {
	name     string
	handles  map[string]*shell.Handle
	lastOpts shell.SpawnOpts
	events   []string
}

func (p *testShellProvider) Name() string { return p.name }

func (p *testShellProvider) Spawn(ctx context.Context, opts shell.SpawnOpts) (*shell.Handle, error) {
	p.lastOpts = opts
	p.events = append(p.events, "spawn")
	return &shell.Handle{ID: "h-1", State: 1, Image: opts.Image, Meta: map[string]any{}}, nil
}

func (p *testShellProvider) Exec(ctx context.Context, handle *shell.Handle, cmd string) (*shell.ExecResult, error) {
	return &shell.ExecResult{Stdout: "ok: " + cmd, ExitCode: 0}, nil
}

func (p *testShellProvider) Read(ctx context.Context, handle *shell.Handle, path string) ([]byte, error) {
	return []byte("content:" + path), nil
}

func (p *testShellProvider) Write(ctx context.Context, handle *shell.Handle, path string, content []byte) error {
	p.events = append(p.events, "write:"+path)
	return nil
}

func (p *testShellProvider) Destroy(ctx context.Context, handle *shell.Handle) error {
	p.events = append(p.events, "destroy")
	return nil
}

func (p *testShellProvider) Alive(handle *shell.Handle) bool { return false }

func newTestShellProvider(name string) *testShellProvider {
	return &testShellProvider{name: name}
}

// TestShellSpawnCheckoutMaterializes: a spawn carrying project+ref checks the
// snapshot out into the first volume's container directory BEFORE the handle
// reports ready (spawn → writes → response), and volumes reach the provider.
func TestShellSpawnCheckoutMaterializes(t *testing.T) {
	fm := testFileManager(t)
	// Seed a project under user u1's scope and commit it.
	fctx := session.WithUserUUID(context.Background(), "u1")
	if _, err := fm.Put(fctx, "user:u1", "proj", "src/main.go", strings.NewReader("package main"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := fm.Put(fctx, "user:u1", "proj", "README.md", strings.NewReader("# p"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := fm.Commit(fctx, "user:u1", "proj", "main", "init"); err != nil {
		t.Fatal(err)
	}

	tp := newTestShellProvider("docker")
	mgr := shell.NewManager([]shell.ShellProvider{tp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := ShellHandlers(mgr, fm, "tester")
	ctx := session.WithOwner(fctx, "session-1")

	resp, ok := h["shell.spawn"](ctx, session.Op{
		Type:    "shell.spawn",
		Project: "proj",
		Ref:     "main",
		Volumes: []string{"/host/work:/work"},
	})
	if !ok {
		t.Fatalf("spawn with checkout failed: %s", resp)
	}
	if len(tp.lastOpts.Volumes) != 1 || tp.lastOpts.Volumes[0] != "/host/work:/work" {
		t.Fatalf("volumes not plumbed: %+v", tp.lastOpts.Volumes)
	}
	// spawn first, then both files materialized into /work, nothing destroyed.
	want := []string{"spawn", "write:/work/README.md", "write:/work/src/main.go"}
	if strings.Join(tp.events, ",") != strings.Join(want, ",") {
		t.Fatalf("event order %v, want %v", tp.events, want)
	}
}

// TestShellSpawnCheckoutRequiresVolume: checkout without a mount target is a
// clear error, not a silent no-op.
func TestShellSpawnCheckoutRequiresVolume(t *testing.T) {
	fm := testFileManager(t)
	tp := newTestShellProvider("docker")
	mgr := shell.NewManager([]shell.ShellProvider{tp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := ShellHandlers(mgr, fm, "tester")
	ctx := session.WithOwner(context.Background(), "session-1")

	resp, ok := h["shell.spawn"](ctx, session.Op{Type: "shell.spawn", Project: "proj"})
	if ok || !strings.Contains(resp, "requires a volume mount") {
		t.Fatalf("expected volume-mount error, got ok=%v %s", ok, resp)
	}
	if len(tp.events) != 0 {
		t.Fatalf("no container may be spawned when validation fails: %v", tp.events)
	}
}

// A checkout that cannot resolve — a project with no commits yet, which is the
// ordinary first-task path — fails the spawn with the word "checkout" in the
// message, and leaves no half-prepared container behind.
//
// The phrase is a contract with a consumer outside this repo: a deployment's
// cold-start retry matches on it to tell "nothing to check out yet" from a
// broken shell and respawns bare. This test is what makes a reword a failure
// here rather than a silent degradation there.
func TestShellSpawnCheckoutFailureNamesCheckout(t *testing.T) {
	fm := testFileManager(t)
	tp := newTestShellProvider("docker")
	mgr := shell.NewManager([]shell.ShellProvider{tp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := ShellHandlers(mgr, fm, "tester")
	ctx := session.WithOwner(context.Background(), "session-1")

	resp, ok := h["shell.spawn"](ctx, session.Op{
		Type:    "shell.spawn",
		Project: "never-committed",
		Ref:     "main",
		Volumes: []string{"/host/work:/work"},
	})
	if ok {
		t.Fatalf("a checkout that cannot resolve must fail the spawn: %s", resp)
	}
	if !strings.Contains(resp, "checkout") {
		t.Fatalf("the failure no longer names the checkout; a retry matching on that word would stop falling back: %s", resp)
	}
	// The container spawned for the attempt is destroyed, not left running: the
	// retry that follows starts from nothing.
	if got := strings.Join(tp.events, ","); got != "spawn,destroy" {
		t.Fatalf("events = %v, want spawn then destroy", tp.events)
	}
}

// TestShellSpawnScratchMount: the owning session's scratch lands in the given
// container directory before ready.
func TestShellSpawnScratchMount(t *testing.T) {
	fm := testFileManager(t)
	if _, err := fm.ScratchPut(context.Background(), "sess:X", "notes.txt", strings.NewReader("n"), "text/plain"); err != nil {
		t.Fatal(err)
	}

	tp := newTestShellProvider("docker")
	mgr := shell.NewManager([]shell.ShellProvider{tp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := ShellHandlers(mgr, fm, "tester")
	ctx := session.WithOwner(context.Background(), "session-1")

	resp, ok := h["shell.spawn"](ctx, session.Op{
		Type:         "shell.spawn",
		Owner:        "sess:X",
		ScratchMount: "/scratch",
	})
	if !ok {
		t.Fatalf("spawn with scratch mount failed: %s", resp)
	}
	want := []string{"spawn", "write:/scratch/notes.txt"}
	if strings.Join(tp.events, ",") != strings.Join(want, ",") {
		t.Fatalf("event order %v, want %v", tp.events, want)
	}
}

// TestVolumeContainerDir: docker -v specs parse to their container path on
// Unix hosts, Windows hosts, and named volumes with options.
func TestVolumeContainerDir(t *testing.T) {
	cases := map[string]string{
		"/host/work:/work":          "/work",
		"/h:/work:ro":               "/work",
		`C:\data\proj:/work`:        "/work",
		`C:\data\proj:/work:ro`:     "/work",
		"named-volume:/srv/data":    "/srv/data",
		"named-volume:/srv/data:ro": "/srv/data",
		"no-container-path":         "",
	}
	for spec, want := range cases {
		if got := volumeContainerDir(spec); got != want {
			t.Errorf("volumeContainerDir(%q) = %q, want %q", spec, got, want)
		}
	}
}
