package shell

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// testProvider is a ShellProvider that records what it is asked to do, in
// place of a real remote machine.
type testProvider struct {
	name      string
	spawned   []SpawnOpts
	execCmds  []string
	destroyed []string
}

func newTestProvider(name string) *testProvider { return &testProvider{name: name} }

func (p *testProvider) Name() string { return p.name }

func (p *testProvider) Spawn(ctx context.Context, opts SpawnOpts) (*Handle, error) {
	p.spawned = append(p.spawned, opts)
	return &Handle{ID: fmt.Sprintf("h-%s-%d", p.name, len(p.spawned)), State: HandleRunning, internal: "test-connection"}, nil
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

func (p *testProvider) Alive(handle *Handle) bool { return handle.State == HandleRunning }

func TestManagerSpawnExecReap(t *testing.T) {
	tp := newTestProvider("ssh")
	mgr := NewManager([]ShellProvider{tp}, nil)
	ctx := context.Background()

	h, err := mgr.Spawn(ctx, "session-1", "ssh", SpawnOpts{Host: "box:22", User: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if h.State != HandleRunning || h.Provider != "ssh" {
		t.Fatalf("handle = %+v, want a running ssh handle", h)
	}
	res, err := mgr.Exec(ctx, "session-1", h.ID, "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "echo hello") {
		t.Fatalf("exec result = %+v", res)
	}
	content, err := mgr.Read(ctx, "session-1", h.ID, "/tmp/file.txt")
	if err != nil || string(content) != "content:/tmp/file.txt" {
		t.Fatalf("read = %q, %v", content, err)
	}
	if mgr.Owned("session-1") != 1 || mgr.Owned("session-2") != 0 {
		t.Fatal("Owned does not count the session's handle")
	}

	mgr.ReapSession(ctx, "session-1")
	if len(tp.destroyed) != 1 {
		t.Fatalf("expected 1 destroy, got %d", len(tp.destroyed))
	}
	if mgr.Owned("session-1") != 0 || mgr.Current("session-1") != nil {
		t.Fatal("a reaped session still owns a handle")
	}
	if _, err := mgr.Exec(ctx, "session-1", h.ID, "echo"); err == nil || !strings.Contains(err.Error(), "spawn a new one") {
		t.Fatalf("exec on a reaped handle = %v, want an error that says what to do", err)
	}
}

func TestManagerOwnershipEnforcement(t *testing.T) {
	tp := newTestProvider("ssh")
	mgr := NewManager([]ShellProvider{tp}, nil)
	ctx := context.Background()
	h, _ := mgr.Spawn(ctx, "alice", "ssh", SpawnOpts{})

	if _, err := mgr.Exec(ctx, "bob", h.ID, "echo hi"); err == nil {
		t.Fatal("bob ran a command on alice's handle")
	}
	if err := mgr.Destroy(ctx, "bob", h.ID); err == nil {
		t.Fatal("bob destroyed alice's handle")
	}
	if _, err := mgr.Exec(ctx, "alice", h.ID, "echo hi"); err != nil {
		t.Fatal(err)
	}
}

func TestManagerEnsureReusesAndDestroyForgets(t *testing.T) {
	tp := newTestProvider("ssh")
	mgr := NewManager([]ShellProvider{tp}, nil)
	ctx := context.Background()

	first, err := mgr.Ensure(ctx, "s", "ssh", SpawnOpts{})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := mgr.Ensure(ctx, "s", "ssh", SpawnOpts{})
	if again.ID != first.ID || len(tp.spawned) != 1 {
		t.Fatalf("Ensure spawned %d handles for one session", len(tp.spawned))
	}
	if err := mgr.Destroy(ctx, "s", first.ID); err != nil {
		t.Fatal(err)
	}
	next, _ := mgr.Ensure(ctx, "s", "ssh", SpawnOpts{})
	if next.ID == first.ID || len(tp.spawned) != 2 {
		t.Fatal("Ensure handed back a destroyed handle")
	}
}

func TestManagerUnknownProvider(t *testing.T) {
	mgr := NewManager([]ShellProvider{}, nil)
	if _, err := mgr.Spawn(context.Background(), "s", "docker", SpawnOpts{}); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
