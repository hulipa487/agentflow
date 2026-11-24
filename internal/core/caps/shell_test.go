package caps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/core/files"
	"agentflow/internal/core/session"
	"agentflow/internal/core/shelljobs"
	"agentflow/internal/core/shellops"
	"agentflow/internal/core/workspace"
	"agentflow/internal/drivers/sandbox"
	"agentflow/internal/drivers/shell"
)

// ---- sandbox shells ---------------------------------------------------------

// fakeSandbox runs a Go function against the job's workspace in place of a
// command in gVisor, and records the specs it was given.
type fakeSandbox struct {
	run func(ctx context.Context, spec sandbox.Spec) (*sandbox.Result, error)

	mu    sync.Mutex
	specs []sandbox.Spec
}

func (f *fakeSandbox) Available() error { return nil }

func (f *fakeSandbox) Run(ctx context.Context, spec sandbox.Spec) (*sandbox.Result, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.mu.Unlock()
	if f.run == nil {
		return &sandbox.Result{Stdout: "ran: " + spec.Command}, nil
	}
	return f.run(ctx, spec)
}

// ran returns the specs seen so far.
func (f *fakeSandbox) ran() []sandbox.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sandbox.Spec{}, f.specs...)
}

func (f *fakeSandbox) reset() {
	f.mu.Lock()
	f.specs = nil
	f.mu.Unlock()
}

// decodeJSON reads an op's successful response. Unlike decodeOK it does not
// ask for an ok field: spawn's result never had one.
func decodeJSON(t *testing.T, resp string, ok bool) map[string]any {
	t.Helper()
	if !ok {
		t.Fatalf("op failed: %s", resp)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(resp), &m); err != nil {
		t.Fatalf("resp not JSON: %v (%s)", err, resp)
	}
	return m
}

type sandboxFixture struct {
	fm     *files.Manager
	box    *fakeSandbox
	front  *shellops.Front
	h      map[string]session.OpHandler
	inbox  chan session.Message
	ctx    context.Context
	target string // who delivered messages were addressed to
}

func newSandboxFixture(t *testing.T) *sandboxFixture {
	t.Helper()
	fm := testFileManager(t)
	box := &fakeSandbox{}
	envs := sandbox.NewEnvs(t.TempDir(), map[string]sandbox.EnvSpec{
		"python": {Rootfs: t.TempDir()}, "node": {Rootfs: t.TempDir()},
	}, nil)
	jobs := shelljobs.New(shelljobs.Config{}, box, envs, workspace.New(t.TempDir(), fm, nil), fm, nil)
	f := &sandboxFixture{fm: fm, box: box, inbox: make(chan session.Message, 4)}
	f.front = &shellops.Front{SSH: shell.NewManager(nil, nil), Jobs: jobs, Files: fm}
	f.h = ShellHandlers(f.front, "coder", func(owner string, msg session.Message) {
		f.target = owner
		f.inbox <- msg
	})
	ctx := session.WithPersonalIdentity(context.Background(), "u1")
	ctx = session.WithOwner(ctx, "coder|s1")
	f.ctx = session.WithShell(ctx, map[string]any{"provider": "sandbox", "environment": "python", "project": "site"})
	return f
}

func (f *sandboxFixture) call(t *testing.T, op session.Op) map[string]any {
	t.Helper()
	resp, ok := f.h[op.Type](f.ctx, op)
	return decodeJSON(t, resp, ok)
}

func (f *sandboxFixture) fails(t *testing.T, op session.Op, want string) {
	t.Helper()
	resp, ok := f.h[op.Type](f.ctx, op)
	if ok || !strings.Contains(resp, want) {
		t.Fatalf("%s = ok:%v %s; want a failure mentioning %q", op.Type, ok, resp, want)
	}
}

func (f *sandboxFixture) stored(t *testing.T, project, path string) string {
	t.Helper()
	e, err := f.fm.Get(context.Background(), "user:u1", project, path)
	if err != nil {
		return ""
	}
	b, _ := f.fm.ReadBlob(context.Background(), e.Handle, 1<<20)
	return string(b)
}

func TestShellRunIsOneJobOnTheProject(t *testing.T) {
	f := newSandboxFixture(t)
	if _, err := f.fm.Put(context.Background(), "user:u1", "site", "in.txt", strings.NewReader("given"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	f.box.run = func(_ context.Context, spec sandbox.Spec) (*sandbox.Result, error) {
		in, _ := os.ReadFile(filepath.Join(spec.Workspace, "in.txt"))
		_ = os.WriteFile(filepath.Join(spec.Workspace, "out.txt"), []byte("from "+string(in)), 0o644)
		return &sandbox.Result{Stdout: "done\n", ExitCode: 2, Duration: 1500 * time.Millisecond}, nil
	}
	res := f.call(t, session.Op{Type: "shell.run", Cmd: "make", ShellEnv: map[string]string{"CI": "1"}, Timeout: 90})

	if res["ok"] != true || res["stdout"] != "done\n" || res["exit_code"] != float64(2) || res["duration_ms"] != float64(1500) {
		t.Errorf("result = %v", res)
	}
	if res["timed_out"] != false || res["project"] != "site" || res["environment"] != "python" {
		t.Errorf("result = %v", res)
	}
	if got := fmt.Sprint(res["changed"]); got != "[out.txt]" {
		t.Errorf("changed = %s", got)
	}
	// Empty lists are lists, so a loop can use # on them without a nil check.
	for _, key := range []string{"deleted", "conflicts", "skipped"} {
		if list, ok := res[key].([]any); !ok || len(list) != 0 {
			t.Errorf("%s = %#v, want an empty list", key, res[key])
		}
	}
	if got := f.stored(t, "site", "out.txt"); got != "from given" {
		t.Errorf("out.txt in the store = %q", got)
	}
	spec := f.box.ran()[0]
	if spec.Command != "make" || spec.Env["CI"] != "1" || spec.Timeout != 90*time.Second {
		t.Errorf("spec = %+v", spec)
	}
}

// Loops written for the old container shell call spawn, exec and destroy. They
// keep working: the handle is the options, and each exec is a job.
func TestShellSpawnExecDestroyStillWorkWithoutAContainer(t *testing.T) {
	f := newSandboxFixture(t)
	for _, provider := range []string{"", "sandbox", "docker"} {
		spawn := f.call(t, session.Op{Type: "shell.spawn", ShellProvider: provider, Project: "other",
			Volumes: []string{"/host/work:/work"}, WorkDir: "/work", ShellEnv: map[string]string{"A": "1"}})
		id, _ := spawn["id"].(string)
		if !shellops.IsSandboxHandle(id) || spawn["provider"] != "sandbox" || spawn["state"] != float64(1) {
			t.Fatalf("spawn(provider=%q) = %v", provider, spawn)
		}
		if len(f.box.ran()) != 0 {
			t.Fatal("spawn started a sandbox; a handle is only options")
		}

		res := f.call(t, session.Op{Type: "shell.exec", ShellHandle: id, Cmd: "ls"})
		if res["stdout"] != "ran: ls" || res["project"] != "other" {
			t.Errorf("exec = %v", res)
		}
		specs := f.box.ran()
		spec := specs[len(specs)-1]
		if spec.Mount != "/work" || spec.WorkDir != "" || spec.Env["A"] != "1" {
			t.Errorf("spec = mount %q workdir %q env %v; the old container path should be the mount point", spec.Mount, spec.WorkDir, spec.Env)
		}

		if resp, ok := f.h["shell.write"](f.ctx, session.Op{Type: "shell.write", ShellHandle: id, Path: "/work/notes/a.txt", Content: "hi"}); !ok || resp != "true" {
			t.Fatalf("write = %s", resp)
		}
		if got := f.stored(t, "other", "notes/a.txt"); got != "hi" {
			t.Errorf("shell.write stored %q under notes/a.txt", got)
		}
		if resp, ok := f.h["shell.destroy"](f.ctx, session.Op{Type: "shell.destroy", ShellHandle: id}); !ok || resp != "true" {
			t.Fatalf("destroy = %s", resp)
		}
		f.box.reset()
	}
}

func TestShellHandleCarriesNoStateAndWorksAnywhere(t *testing.T) {
	a, b := newSandboxFixture(t), newSandboxFixture(t)
	spawn := a.call(t, session.Op{Type: "shell.spawn", Project: "site", Image: "node", WorkDir: "app"})
	id := spawn["id"].(string)
	// Another engine instance — nothing shared but the id — runs a command
	// with it. This is the failover case the old handle registry existed for.
	if _, err := b.fm.Put(context.Background(), "user:u1", "site", "app/x", strings.NewReader("1"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	res := b.call(t, session.Op{Type: "shell.exec", ShellHandle: id, Cmd: "pwd"})
	if res["environment"] != "node" || b.box.ran()[0].WorkDir != "app" {
		t.Errorf("the handle's options did not travel: %v, workdir %q", res, b.box.ran()[0].WorkDir)
	}
	b.fails(t, session.Op{Type: "shell.exec", ShellHandle: "sandbox:not-base64!", Cmd: "x"}, "is not one shell.spawn returned")
}

func TestShellOptionsThatASandboxCannotHonourAreRefused(t *testing.T) {
	f := newSandboxFixture(t)
	for _, kind := range []string{"shell.spawn", "shell.run", "shell.start"} {
		f.fails(t, session.Op{Type: kind, Cmd: "x", Net: "bridge"}, "sandbox jobs run with no network")
		f.fails(t, session.Op{Type: kind, Cmd: "x", Ref: "v1.2"}, "runs on the project's working tree")
		f.fails(t, session.Op{Type: kind, Cmd: "x", ScratchMount: "/scratch"}, "scratch_mount is not available")
		f.fails(t, session.Op{Type: kind, Cmd: "x", Image: "ubuntu:24.04"}, "is not a declared environment (declared: node, python)")
		f.fails(t, session.Op{Type: kind, Cmd: "x", WorkDir: "/etc"}, "outside the workspace")
		f.fails(t, session.Op{Type: kind, Cmd: "x", Volumes: []string{"nopath"}}, "has no container path")
	}
	f.fails(t, session.Op{Type: "shell.spawn", ShellProvider: "mesos"}, "unknown shell provider")
	if n := len(f.box.ran()); n != 0 {
		t.Errorf("%d job(s) ran for refused options", n)
	}
	// "none" and "main" are the two values that mean what a job does anyway.
	f.call(t, session.Op{Type: "shell.run", Cmd: "x", Net: "none", Ref: "main"})
}

func TestShellWriteStaysInTheWorkspace(t *testing.T) {
	f := newSandboxFixture(t)
	id := f.call(t, session.Op{Type: "shell.spawn"})["id"].(string)
	f.fails(t, session.Op{Type: "shell.write", ShellHandle: id, Path: "/etc/passwd", Content: "x"}, "outside the workspace")
	f.fails(t, session.Op{Type: "shell.write", ShellHandle: id, Path: "../up", Content: "x"}, "not a file inside the workspace")
	if resp, ok := f.h["shell.write"](f.ctx, session.Op{Type: "shell.write", ShellHandle: id, Path: "/workspace/a.txt", Content: "x"}); !ok {
		t.Fatalf("write to the workspace failed: %s", resp)
	}
	if got := f.stored(t, "site", "a.txt"); got != "x" {
		t.Errorf("a.txt = %q", got)
	}
}

func TestShellStartDeliversItsResultToTheInboxAsTheUser(t *testing.T) {
	f := newSandboxFixture(t)
	release := make(chan struct{})
	f.box.run = func(_ context.Context, spec sandbox.Spec) (*sandbox.Result, error) {
		spec.OnOutput("stdout", []byte("building\n"))
		<-release
		_ = os.WriteFile(filepath.Join(spec.Workspace, "bin"), []byte("x"), 0o755)
		return &sandbox.Result{ExitCode: 0}, nil
	}
	started := f.call(t, session.Op{Type: "shell.start", Cmd: "make all"})
	id, _ := started["id"].(string)
	if id == "" || started["state"] != "running" {
		t.Fatalf("start = %v", started)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		st := f.call(t, session.Op{Type: "shell.status", JobID: id})
		if st["stdout"] == "building\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status never showed the job's output: %v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(release)
	var msg session.Message
	select {
	case msg = <-f.inbox:
	case <-time.After(5 * time.Second):
		t.Fatal("no result was delivered")
	}
	if f.target != "coder|s1" || msg.Type != "shell.result" || msg.ID != "shell:"+id || msg.From != "system:shell" {
		t.Errorf("message = %+v, delivered to %q", msg, f.target)
	}
	if msg.Payload["state"] != "finished" || fmt.Sprint(msg.Payload["changed"]) != "[bin]" {
		t.Errorf("payload = %v", msg.Payload)
	}
	// The result turn must run as the user who started the job, and must not
	// be an engine-privileged turn.
	p := msg.Provenance
	if p == nil || p.Kind != "shell" || p.UserUUID == nil || *p.UserUUID != "u1" {
		t.Fatalf("provenance = %+v, want kind shell stamped with the starting user", p)
	}
	if p.Kind == "system" || p.Kind == "scheduler" {
		t.Fatal("a job result arrived with maintenance provenance")
	}

	// Another session cannot see or stop it.
	other := session.WithOwner(f.ctx, "coder|s2")
	if resp, ok := f.h["shell.status"](other, session.Op{Type: "shell.status", JobID: id}); ok {
		t.Errorf("another session read the job: %s", resp)
	}
}

func TestShellCancelStopsABackgroundJob(t *testing.T) {
	f := newSandboxFixture(t)
	f.box.run = func(ctx context.Context, _ sandbox.Spec) (*sandbox.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	id := f.call(t, session.Op{Type: "shell.start", Cmd: "sleep 600"})["id"].(string)
	for len(f.box.ran()) == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := f.call(t, session.Op{Type: "shell.cancel", JobID: id}); got["state"] != "cancelled" {
		t.Errorf("cancel = %v", got)
	}
	if msg := <-f.inbox; msg.Payload["state"] != "cancelled" {
		t.Errorf("delivered payload = %v", msg.Payload)
	}
}

func TestShellSpawnRejectsSSHProvider(t *testing.T) {
	f := newSandboxFixture(t)
	if resp, ok := f.h["shell.spawn"](f.ctx, session.Op{Type: "shell.spawn", ShellProvider: "ssh"}); ok || !strings.Contains(resp, "ssh.spawn") {
		t.Fatalf("shell.spawn with ssh provider should direct to ssh.spawn, got ok=%v %s", ok, resp)
	}
}

func TestShellHandlerNoOwner(t *testing.T) {
	f := newSandboxFixture(t)
	for kind := range f.h {
		if resp, ok := f.h[kind](context.Background(), session.Op{Type: kind, Cmd: "x"}); ok || !strings.Contains(resp, "no owner") {
			t.Errorf("%s without an owner = ok:%v %s", kind, ok, resp)
		}
	}
}

// ---- remote (SSH) shells ----------------------------------------------------

// testShellProvider is a minimal ShellProvider standing in for a remote
// machine. events records the call order (spawn/write/destroy) so
// materialization tests can assert what happened before the handle reported
// ready.
type testShellProvider struct {
	name     string
	lastOpts shell.SpawnOpts
	events   []string
}

func (p *testShellProvider) Name() string { return p.name }

func (p *testShellProvider) Spawn(ctx context.Context, opts shell.SpawnOpts) (*shell.Handle, error) {
	p.lastOpts = opts
	p.events = append(p.events, "spawn")
	return &shell.Handle{ID: "h-1", State: shell.HandleRunning, Meta: map[string]any{}}, nil
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

func sshHandlers(t *testing.T, fm *files.Manager) (*testShellProvider, map[string]session.OpHandler) {
	t.Helper()
	tp := &testShellProvider{name: "ssh"}
	front := &shellops.Front{SSH: shell.NewManager([]shell.ShellProvider{tp}, nil), Files: fm}
	return tp, SSHHandlers(front, "tester")
}

func TestSSHShellKeepsRealHandles(t *testing.T) {
	tp, h := sshHandlers(t, nil)
	ctx := session.WithOwner(context.Background(), "session-1")

	resp, ok := h["ssh.spawn"](ctx, session.Op{Type: "ssh.spawn", Host: "box:22", User: "dev",
		ShellOpts: map[string]any{"region": "ewr"}})
	spawn := decodeJSON(t, resp, ok)
	handleID, _ := spawn["id"].(string)
	if handleID != "h-1" || spawn["provider"] != "ssh" {
		t.Fatalf("spawn = %v", spawn)
	}
	if tp.lastOpts.Host != "box:22" || tp.lastOpts.User != "dev" || tp.lastOpts.ShellOpts["region"] != "ewr" {
		t.Fatalf("spawn options not plumbed: %+v", tp.lastOpts)
	}

	resp, ok = h["ssh.exec"](ctx, session.Op{Type: "ssh.exec", ShellHandle: handleID, Cmd: "echo hello"})
	var execResult struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	if !ok || json.Unmarshal([]byte(resp), &execResult) != nil || execResult.Stdout != "ok: echo hello" {
		t.Fatalf("exec = %s", resp)
	}
	if resp, ok = h["ssh.write"](ctx, session.Op{Type: "ssh.write", ShellHandle: handleID, Path: "/tmp/a.txt", Content: "hi"}); !ok || resp != "true" {
		t.Fatalf("ssh.write failed: ok=%v %s", ok, resp)
	}
	resp, ok = h["ssh.read"](ctx, session.Op{Type: "ssh.read", ShellHandle: handleID, Path: "/tmp/a.txt"})
	readRes := decodeJSON(t, resp, ok)
	if readRes["ok"] != true || readRes["path"] != "/tmp/a.txt" || readRes["content"] != "content:/tmp/a.txt" {
		t.Fatalf("ssh.read = %v", readRes)
	}
	if resp, ok = h["ssh.close"](ctx, session.Op{Type: "ssh.close", ShellHandle: handleID}); !ok || resp != "true" {
		t.Fatalf("ssh.close failed: %s", resp)
	}
}

func TestSSHSpawnInheritsContextProfile(t *testing.T) {
	tp, h := sshHandlers(t, nil)
	ctx := session.WithSSH(session.WithOwner(context.Background(), "session-1"), map[string]any{
		"host": "bound:22", "user": "alice", "workdir": "/srv/app", "env": map[string]string{"A": "1", "B": "2"},
	})
	resp, ok := h["ssh.spawn"](ctx, session.Op{Type: "ssh.spawn", User: "bob", ShellEnv: map[string]string{"B": "override", "C": "3"}})
	if !ok {
		t.Fatalf("ssh.spawn failed: %s", resp)
	}
	if tp.lastOpts.Host != "bound:22" || tp.lastOpts.User != "bob" || tp.lastOpts.WorkDir != "/srv/app" {
		t.Fatalf("merged opts = %+v", tp.lastOpts)
	}
	if tp.lastOpts.Env["A"] != "1" || tp.lastOpts.Env["B"] != "override" || tp.lastOpts.Env["C"] != "3" {
		t.Fatalf("merged env = %v", tp.lastOpts.Env)
	}
}

func TestShellAndSSHOpsRejectCrossHandles(t *testing.T) {
	f := newSandboxFixture(t)
	_, sshH := sshHandlers(t, nil)
	sbID, _ := (shellops.Options{}).Encode()

	for _, op := range []string{"shell.exec", "shell.write", "shell.destroy"} {
		if resp, ok := f.h[op](f.ctx, session.Op{Type: op, ShellHandle: "h-1", Cmd: "ls", Path: "a.txt", Content: "x"}); ok || !strings.Contains(resp, "ssh.") {
			t.Fatalf("%s accepted SSH handle: ok=%v %s", op, ok, resp)
		}
	}
	for _, op := range []string{"ssh.exec", "ssh.read", "ssh.write", "ssh.close"} {
		if resp, ok := sshH[op](f.ctx, session.Op{Type: op, ShellHandle: sbID, Cmd: "ls", Path: "a.txt", Content: "x"}); ok || !strings.Contains(resp, "shell.*") {
			t.Fatalf("%s accepted sandbox handle: ok=%v %s", op, ok, resp)
		}
	}
}

// TestShellSpawnCheckoutMaterializes: an SSH spawn carrying project+ref copies
// the snapshot into the first volume's remote directory BEFORE the handle
// reports ready (spawn → writes → response).
func TestShellSpawnCheckoutMaterializes(t *testing.T) {
	fm := testFileManager(t)
	fctx := session.WithPersonalIdentity(context.Background(), "u1")
	if _, err := fm.Put(fctx, "user:u1", "proj", "src/main.go", strings.NewReader("package main"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := fm.Put(fctx, "user:u1", "proj", "README.md", strings.NewReader("# p"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := fm.Commit(fctx, "user:u1", "proj", "main", "init"); err != nil {
		t.Fatal(err)
	}
	tp, h := sshHandlers(t, fm)
	ctx := session.WithOwner(fctx, "session-1")

	resp, ok := h["ssh.spawn"](ctx, session.Op{Type: "ssh.spawn",
		Project: "proj", Ref: "main", Volumes: []string{"/host/work:/work"}})
	if !ok {
		t.Fatalf("spawn with checkout failed: %s", resp)
	}
	want := []string{"spawn", "write:/work/README.md", "write:/work/src/main.go"}
	if strings.Join(tp.events, ",") != strings.Join(want, ",") {
		t.Fatalf("event order %v, want %v", tp.events, want)
	}
}

// TestShellSpawnCheckoutRequiresVolume: checkout without a target directory is
// a clear error, not a silent no-op.
func TestShellSpawnCheckoutRequiresVolume(t *testing.T) {
	tp, h := sshHandlers(t, testFileManager(t))
	ctx := session.WithOwner(context.Background(), "session-1")

	resp, ok := h["ssh.spawn"](ctx, session.Op{Type: "ssh.spawn", Project: "proj"})
	if ok || !strings.Contains(resp, "requires a volume mount") {
		t.Fatalf("expected volume-mount error, got ok=%v %s", ok, resp)
	}
	if len(tp.events) != 0 {
		t.Fatalf("no connection may be opened when validation fails: %v", tp.events)
	}
}

// A checkout that cannot resolve — a project with no commits yet, which is the
// ordinary first-task path — fails the spawn with the word "checkout" in the
// message, and leaves no half-prepared connection behind.
//
// The phrase is a contract with a consumer outside this repo: a deployment's
// cold-start retry matches on it to tell "nothing to check out yet" from a
// broken shell and respawns bare. This test is what makes a reword a failure
// here rather than a silent degradation there.
func TestShellSpawnCheckoutFailureNamesCheckout(t *testing.T) {
	tp, h := sshHandlers(t, testFileManager(t))
	ctx := session.WithOwner(context.Background(), "session-1")

	resp, ok := h["ssh.spawn"](ctx, session.Op{Type: "ssh.spawn",
		Project: "never-committed", Ref: "main", Volumes: []string{"/host/work:/work"}})
	if ok {
		t.Fatalf("a checkout that cannot resolve must fail the spawn: %s", resp)
	}
	if !strings.Contains(resp, "checkout") {
		t.Fatalf("the failure no longer names the checkout; a retry matching on that word would stop falling back: %s", resp)
	}
	if got := strings.Join(tp.events, ","); got != "spawn,destroy" {
		t.Fatalf("events = %v, want spawn then destroy", tp.events)
	}
}

// TestShellSpawnScratchMount: the owning session's scratch lands in the given
// remote directory before ready.
func TestShellSpawnScratchMount(t *testing.T) {
	fm := testFileManager(t)
	if _, err := fm.ScratchPut(context.Background(), "sess:X", "notes.txt", strings.NewReader("n"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	tp, h := sshHandlers(t, fm)
	ctx := session.WithOwner(context.Background(), "session-1")

	resp, ok := h["ssh.spawn"](ctx, session.Op{Type: "ssh.spawn", Owner: "sess:X", ScratchMount: "/scratch"})
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
