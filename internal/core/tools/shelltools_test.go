package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/files"
	"agentflow/internal/core/media"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
	"agentflow/internal/core/shelljobs"
	"agentflow/internal/core/shellops"
	"agentflow/internal/core/workspace"
	"agentflow/internal/drivers/sandbox"
	"agentflow/internal/drivers/shell"
)

// fakeShellProvider stands in for a remote machine: it records spawns, execs,
// writes, and destroys, and answers reads with deterministic content.
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
	return &shell.Handle{ID: fmt.Sprintf("h-%d", p.spawns), State: shell.HandleRunning}, nil
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

// fakeRunner runs a Go function against a job's workspace in place of a
// command in gVisor.
type fakeRunner struct {
	mu          sync.Mutex
	specs       []sandbox.Spec
	run         func(spec sandbox.Spec) *sandbox.Result
	unavailable error
}

func (f *fakeRunner) Available() error { return f.unavailable }
func (f *fakeRunner) Run(ctx context.Context, spec sandbox.Spec) (*sandbox.Result, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.mu.Unlock()
	if f.run != nil {
		return f.run(spec), nil
	}
	return &sandbox.Result{Stdout: "ran: " + spec.Command, Duration: 7 * time.Millisecond}, nil
}

type shellFixture struct {
	r      *Registry
	as     *AgentSet
	ssh    *fakeShellProvider
	runner *fakeRunner
	fm     *files.Manager
}

func shellToolSetup(t *testing.T) *shellFixture {
	t.Helper()
	blobs, err := media.Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runtime.OpenSQLite(filepath.Join(t.TempDir(), "rt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	f := &shellFixture{ssh: &fakeShellProvider{name: "ssh"}, runner: &fakeRunner{}}
	f.fm = files.New(blobs, rt, time.Hour, 1<<20, quiet())
	envs := sandbox.NewEnvs(t.TempDir(), map[string]sandbox.EnvSpec{"python": {Rootfs: t.TempDir()}}, nil)
	jobs := shelljobs.New(shelljobs.Config{}, f.runner, envs, workspace.New(t.TempDir(), f.fm, nil), f.fm, nil)
	f.r = NewRegistry()
	RegisterShellBuiltins(f.r, &shellops.Front{
		SSH: shell.NewManager([]shell.ShellProvider{f.ssh}, nil), Jobs: jobs, Files: f.fm,
	})
	f.as = f.r.Expose(nil, config.ToolsPolicy{}, false)
	return f
}

func (f *shellFixture) invoke(t *testing.T, ctx context.Context, tool string, args map[string]any) map[string]any {
	t.Helper()
	res, err := f.as.ByName[tool].Invoke(ctx, args)
	if err != nil {
		t.Fatalf("%s: raw error leaked: %v", tool, err)
	}
	return res.(map[string]any)
}

func (f *shellFixture) stored(t *testing.T, scope, project, path string) string {
	t.Helper()
	e, err := f.fm.Get(context.Background(), scope, project, path)
	if err != nil {
		return ""
	}
	b, _ := f.fm.ReadBlob(context.Background(), e.Handle, 1<<20)
	return string(b)
}

// sandboxCtx carries an owner and a bound sandbox shell profile, as the actor
// stamps them for a session of an agent with `shell:` set. There is no user
// in it, so the project lives in the agent's own scope.
func sandboxCtx() context.Context {
	return session.WithShell(session.WithOwner(context.Background(), "main|s1"), map[string]any{
		"provider": "sandbox", "environment": "python", "project": "site",
	})
}

// sshCtx is the same for an agent bound to a remote machine.
func sshCtx() context.Context {
	return session.WithShell(session.WithOwner(context.Background(), "main|s1"), map[string]any{
		"provider": "ssh", "host": "box:22", "user": "dev", "workdir": "/work",
	})
}

// TestShellExecRunsAJobOnTheProject: with a sandbox profile, builtin:shell.exec
// is one job on the profile's project. The confirm gate fires through
// AgentSet.Invoke; the confirmed re-dispatch calls the spec's Invoke directly.
func TestShellExecRunsAJobOnTheProject(t *testing.T) {
	f := shellToolSetup(t)
	ctx := sandboxCtx()

	res, err := f.as.Invoke(ctx, "builtin:shell.exec", map[string]any{"command": "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if m := res.(map[string]any); m["needs_confirm"] != true {
		t.Fatalf("exec should require confirm by default, got %v", m)
	}
	if len(f.runner.specs) != 0 {
		t.Fatal("the confirm gate must not run a job")
	}

	f.runner.run = func(spec sandbox.Spec) *sandbox.Result {
		_ = os.WriteFile(filepath.Join(spec.Workspace, "made.txt"), []byte("by the job"), 0o644)
		return &sandbox.Result{Stdout: "ran: " + spec.Command, ExitCode: 4, Duration: 7 * time.Millisecond}
	}
	m := f.invoke(t, ctx, "builtin:shell.exec", map[string]any{"command": "ls"})
	if m["ok"] != true || m["tool"] != "builtin:shell.exec" || m["stdout"] != "ran: ls" || m["exit_code"] != 4 || m["duration_ms"] != int64(7) {
		t.Fatalf("unexpected exec result: %v", m)
	}
	if got := fmt.Sprint(m["changed"]); got != "[made.txt]" {
		t.Fatalf("changed = %s", got)
	}
	if got := f.stored(t, "agent:main", "site", "made.txt"); got != "by the job" {
		t.Fatalf("the job's file is not in the project: %q", got)
	}
	if f.ssh.spawns != 0 {
		t.Fatal("a sandbox profile opened a remote connection")
	}
}

// TestFileToolsWorkOnTheProjectWithoutASandbox: reading and writing a project
// file does not take running anything.
func TestFileToolsWorkOnTheProjectWithoutASandbox(t *testing.T) {
	f := shellToolSetup(t)
	ctx := sandboxCtx()

	for _, tool := range []string{"builtin:shell.write", "builtin:fs.write"} {
		m := f.invoke(t, ctx, tool, map[string]any{"path": "/workspace/src/" + tool[8:] + ".txt", "content": "hi"})
		if m["ok"] != true || m["path"] != "src/"+tool[8:]+".txt" {
			t.Fatalf("%s = %v", tool, m)
		}
	}
	m := f.invoke(t, ctx, "builtin:fs.read", map[string]any{"path": "src/fs.write.txt"})
	if m["ok"] != true || m["content"] != "hi" || m["truncated"] != false {
		t.Fatalf("fs.read = %v", m)
	}
	if m := f.invoke(t, ctx, "builtin:fs.read", map[string]any{"path": "nope.txt"}); m["ok"] != false {
		t.Fatalf("reading a missing file = %v", m)
	}
	if m := f.invoke(t, ctx, "builtin:fs.write", map[string]any{"path": "/etc/passwd", "content": "x"}); m["ok"] != false || !strings.Contains(fmt.Sprint(m["error"]), "outside the workspace") {
		t.Fatalf("writing outside the workspace = %v", m)
	}
	if m := f.invoke(t, ctx, "builtin:shell.destroy", nil); m["ok"] != true || m["destroyed"] != false {
		t.Fatalf("destroy on a sandbox profile = %v", m)
	}
	if len(f.runner.specs) != 0 {
		t.Fatalf("%d sandbox job(s) ran for file operations", len(f.runner.specs))
	}
}

// TestRemoteShellLazySpawnAndReuse: with an SSH profile the tools keep one
// connection per session, opened on first use from the bound profile.
func TestRemoteShellLazySpawnAndReuse(t *testing.T) {
	f := shellToolSetup(t)
	ctx := sshCtx()

	m := f.invoke(t, ctx, "builtin:shell.exec", map[string]any{"command": "ls"})
	if m["ok"] != true || m["stdout"] != "ran: ls" || m["exit_code"] != 0 || m["duration_ms"] != int64(7) || m["handle_id"] != "h-1" {
		t.Fatalf("unexpected exec result: %v", m)
	}
	if f.ssh.lastOpts.Host != "box:22" || f.ssh.lastOpts.User != "dev" || f.ssh.lastOpts.WorkDir != "/work" {
		t.Fatalf("spawn opts not from the bound profile: %+v", f.ssh.lastOpts)
	}
	if m := f.invoke(t, ctx, "builtin:shell.exec", map[string]any{"command": "pwd"}); m["handle_id"] != "h-1" {
		t.Fatalf("handle not reused: %v", m)
	}
	if f.ssh.spawns != 1 || len(f.ssh.execs) != 2 {
		t.Fatalf("expected reuse (1 spawn, 2 execs), got %d spawns, %d execs", f.ssh.spawns, len(f.ssh.execs))
	}

	m = f.invoke(t, ctx, "builtin:shell.write", map[string]any{"path": "/tmp/a.txt", "content": "hi"})
	if m["ok"] != true || m["handle_id"] != "h-1" || len(f.ssh.writes) != 1 || f.ssh.writes[0] != "/tmp/a.txt=hi" {
		t.Fatalf("write = %v, recorded %v", m, f.ssh.writes)
	}
	// The file tools follow the agent's shell: the remote machine, not a project.
	if m := f.invoke(t, ctx, "builtin:fs.read", map[string]any{"path": "/tmp/a.txt"}); m["content"] != "content:/tmp/a.txt" {
		t.Fatalf("fs.read on a remote shell = %v", m)
	}

	if m := f.invoke(t, ctx, "builtin:shell.destroy", nil); m["ok"] != true || m["destroyed"] != true {
		t.Fatalf("unexpected destroy result: %v", m)
	}
	if m := f.invoke(t, ctx, "builtin:shell.destroy", nil); m["ok"] != true || m["destroyed"] != false {
		t.Fatalf("second destroy should be a no-op, got %v", m)
	}
	if m := f.invoke(t, ctx, "builtin:shell.exec", map[string]any{"command": "ls"}); m["handle_id"] != "h-2" {
		t.Fatalf("exec after destroy should open a fresh connection, got %v", m)
	}
	if len(f.runner.specs) != 0 {
		t.Fatal("an SSH profile ran a sandbox job")
	}
}

// TestShellToolsNoProfileUnavailable: an agent without a bound shell profile
// gets honest-unavailable from every tool — never a raw error, never a job.
// That includes the file tools given a sandbox handle: a handle is only
// options, a model can write one out by hand, and it must not be a way to
// reach a project without a shell.
func TestShellToolsNoProfileUnavailable(t *testing.T) {
	f := shellToolSetup(t)
	ctx := session.WithOwner(context.Background(), "main|s1") // no shell in ctx
	if _, err := f.fm.Put(ctx, "agent:main", "workspace", "secret.txt", strings.NewReader("s3cret"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	forged, err := shellops.Options{}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"builtin:shell.exec", "builtin:shell.write", "builtin:shell.destroy", "builtin:fs.read", "builtin:fs.write"} {
		for _, handle := range []string{"", forged} {
			m := f.invoke(t, ctx, name, map[string]any{"command": "ls", "path": "secret.txt", "content": "y", "handle_id": handle})
			if m["ok"] != false || m["unavailable"] != true {
				t.Fatalf("%s (handle %q): expected honest-unavailable, got %v", name, handle, m)
			}
		}
	}
	if f.ssh.spawns != 0 || len(f.runner.specs) != 0 {
		t.Fatal("the unavailable path must not start anything")
	}
	if got := f.stored(t, "agent:main", "workspace", "secret.txt"); got != "s3cret" {
		t.Fatalf("a tool without a shell profile changed a project file: %q", got)
	}
}

// TestShellExecSaysWhenTheSandboxCannotRun: no gVisor on this host is an
// honest-unavailable result with the reason, not an error.
func TestShellExecSaysWhenTheSandboxCannotRun(t *testing.T) {
	f := shellToolSetup(t)
	f.runner.unavailable = fmt.Errorf("%w: runsc was not found", sandbox.ErrUnavailable)
	m := f.invoke(t, sandboxCtx(), "builtin:shell.exec", map[string]any{"command": "ls"})
	if m["ok"] != false || m["unavailable"] != true || !strings.Contains(fmt.Sprint(m), "runsc was not found") {
		t.Fatalf("exec without a sandbox = %v", m)
	}
}

// TestShellToolsCapabilityGateContract: main.go policy-forbids the shell and
// file tools for agents without the shell.exec capability — this locks the
// names and the Expose behavior that gate relies on.
func TestShellToolsCapabilityGateContract(t *testing.T) {
	f := shellToolSetup(t)
	gated := []string{"builtin:shell.exec", "builtin:shell.write", "builtin:shell.destroy", "builtin:fs.read", "builtin:fs.write"}
	as := f.r.Expose(nil, config.ToolsPolicy{Forbidden: gated}, false)
	for _, name := range gated {
		if _, ok := as.ByName[name]; ok {
			t.Fatalf("%s must be forbidden for agents without shell.exec", name)
		}
		if _, ok := f.as.ByName[name]; !ok {
			t.Fatalf("%s is not registered", name)
		}
	}
	if !f.as.ByName["builtin:fs.write"].NeedsConfirm || f.as.ByName["builtin:fs.read"].NeedsConfirm {
		t.Fatal("fs.write should require confirm, and fs.read should not")
	}
}

// TestFSToolsOnARemoteHandle: a loop that opened a remote shell can hand its
// handle to the file tools.
func TestFSToolsOnARemoteHandle(t *testing.T) {
	f := shellToolSetup(t)
	front := &shellops.Front{SSH: shell.NewManager([]shell.ShellProvider{f.ssh}, nil)}
	h, err := front.SSH.Spawn(context.Background(), "session-1", "ssh", shell.SpawnOpts{})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	RegisterShellBuiltins(r, front)
	as := r.Expose([]string{"builtin:fs.read"}, config.ToolsPolicy{}, false)
	res, err := as.Invoke(session.WithOwner(context.Background(), "session-1"), "builtin:fs.read", map[string]any{"handle_id": h.ID, "path": "/tmp/a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if m := res.(map[string]any); m["ok"] != true || m["content"] != "content:/tmp/a.txt" {
		t.Fatalf("unexpected fs.read result: %v", m)
	}
}
