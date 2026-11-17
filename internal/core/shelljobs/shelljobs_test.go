package shelljobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/core/files"
	"agentflow/internal/core/media"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/workspace"
	"agentflow/internal/drivers/sandbox"
)

// fakeRunner stands in for the sandbox: it runs a Go function against the
// workspace directory instead of a command in gVisor.
type fakeRunner struct {
	mu          sync.Mutex
	specs       []sandbox.Spec
	run         func(ctx context.Context, spec sandbox.Spec) (*sandbox.Result, error)
	unavailable error
	active      atomic.Int32
	peak        atomic.Int32
}

func (f *fakeRunner) Available() error { return f.unavailable }

func (f *fakeRunner) Run(ctx context.Context, spec sandbox.Spec) (*sandbox.Result, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	run := f.run
	f.mu.Unlock()
	n := f.active.Add(1)
	defer f.active.Add(-1)
	for {
		peak := f.peak.Load()
		if n <= peak || f.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	if run == nil {
		return &sandbox.Result{}, nil
	}
	return run(ctx, spec)
}

func (f *fakeRunner) last(t *testing.T) sandbox.Spec {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.specs) == 0 {
		t.Fatal("the runner was never called")
	}
	return f.specs[len(f.specs)-1]
}

type fixture struct {
	t      *testing.T
	fm     *files.Manager
	runner *fakeRunner
	svc    *Service
	rows   runtime.Rows
}

const scope = "user:u1"

func newFixture(t *testing.T, cfg Config) *fixture {
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
	quiet := slog.New(slog.DiscardHandler)
	fm := files.New(blobs, rt, time.Hour, 1<<20, quiet)
	envs := sandbox.NewEnvs(t.TempDir(), map[string]sandbox.EnvSpec{
		"python": {Rootfs: t.TempDir(), Env: map[string]string{"LANG": "C", "FROM": "environment"}},
		"node":   {Rootfs: t.TempDir()},
	}, nil)
	runner := &fakeRunner{}
	svc := New(cfg, runner, envs, workspace.New(filepath.Join(t.TempDir(), "ws"), fm, nil), fm, nil)
	return &fixture{t: t, fm: fm, runner: runner, svc: svc, rows: rt}
}

func (f *fixture) put(project, path, content string) {
	f.t.Helper()
	if _, err := f.fm.Put(context.Background(), scope, project, path, strings.NewReader(content), "text/plain"); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) stored(project, path string) string {
	f.t.Helper()
	e, err := f.fm.Get(context.Background(), scope, project, path)
	if err != nil {
		return ""
	}
	b, _ := f.fm.ReadBlob(context.Background(), e.Handle, 1<<20)
	return string(b)
}

func req(command string) Request {
	return Request{Scope: scope, Owner: "agent|s1", Command: command, Profile: Profile{Environment: "python", Project: "site"}}
}

func TestRunGivesTheCommandTheProjectAndStoresWhatItChanged(t *testing.T) {
	f := newFixture(t, Config{})
	f.put("site", "in.txt", "given")
	f.runner.run = func(_ context.Context, spec sandbox.Spec) (*sandbox.Result, error) {
		in, err := os.ReadFile(filepath.Join(spec.Workspace, "in.txt"))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(spec.Workspace, "out.txt"), []byte("made from "+string(in)), 0o644); err != nil {
			return nil, err
		}
		return &sandbox.Result{Stdout: "ok\n", ExitCode: 3, Duration: time.Second}, nil
	}
	res, err := f.svc.Run(context.Background(), req("make"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Stdout != "ok\n" || res.Project != "site" || res.Environment != "python" {
		t.Errorf("result = %+v", res)
	}
	if got := res.Changes.Changed; len(got) != 1 || got[0] != "out.txt" {
		t.Errorf("changed = %v", got)
	}
	if got := f.stored("site", "out.txt"); got != "made from given" {
		t.Errorf("out.txt in the store = %q", got)
	}
	spec := f.runner.last(t)
	if spec.Command != "make" || spec.Timeout != 60*time.Second || spec.Limits.PIDs != 512 || spec.Limits.MemoryBytes != 1<<30 {
		t.Errorf("spec = %+v, want the command with the default limits", spec)
	}
	if spec.Env["LANG"] != "C" {
		t.Errorf("the environment's variables did not reach the job: %v", spec.Env)
	}
}

func TestSettingsLayerCallOverManifestOverProfile(t *testing.T) {
	f := newFixture(t, Config{MaxTimeout: 5 * time.Minute})
	f.put("site", "app/x", "1")
	f.put("site", "lib/x", "1")
	f.put("site", ManifestPath, "environment: node\nworkdir: app\ntimeout: 90s\nenv: {FROM: manifest, M: \"1\"}\nignore: [\"dist/\"]\n")
	r := req("true")
	r.Profile.WorkDir = "lib"
	r.Profile.Timeout = 10 * time.Second
	r.Profile.Env = map[string]string{"FROM": "profile", "P": "1"}
	r.Profile.MemoryBytes = 256 << 20

	if _, err := f.svc.Run(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	spec := f.runner.last(t)
	if spec.WorkDir != "app" || spec.Timeout != 90*time.Second {
		t.Errorf("workdir %q, timeout %s: the manifest should override the profile", spec.WorkDir, spec.Timeout)
	}
	if spec.Env["FROM"] != "manifest" || spec.Env["P"] != "1" || spec.Env["M"] != "1" {
		t.Errorf("env = %v", spec.Env)
	}
	if spec.Limits.MemoryBytes != 256<<20 {
		t.Errorf("a profile's lower memory limit was not applied: %d", spec.Limits.MemoryBytes)
	}

	r.WorkDir, r.Timeout, r.Environment = "lib", time.Hour, "python"
	r.Env = map[string]string{"FROM": "call"}
	r.Profile.MemoryBytes = 8 << 30
	if _, err := f.svc.Run(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	spec = f.runner.last(t)
	if spec.WorkDir != "lib" || spec.Env["FROM"] != "call" || spec.Env["LANG"] != "C" {
		t.Errorf("the call's settings did not win: workdir %q, env %v", spec.WorkDir, spec.Env)
	}
	if spec.Timeout != 5*time.Minute {
		t.Errorf("timeout = %s, want it capped at the deployment's 5m", spec.Timeout)
	}
	if spec.Limits.MemoryBytes != 1<<30 {
		t.Errorf("a profile raised the memory limit past the deployment's: %d", spec.Limits.MemoryBytes)
	}
}

func TestManifestIgnorePatternsApply(t *testing.T) {
	f := newFixture(t, Config{})
	f.put("site", ManifestPath, "ignore: [\"dist/\"]\n")
	f.runner.run = func(_ context.Context, spec sandbox.Spec) (*sandbox.Result, error) {
		_ = os.MkdirAll(filepath.Join(spec.Workspace, "dist"), 0o755)
		_ = os.WriteFile(filepath.Join(spec.Workspace, "dist", "app.js"), []byte("built"), 0o644)
		_ = os.WriteFile(filepath.Join(spec.Workspace, "kept.txt"), []byte("kept"), 0o644)
		return &sandbox.Result{}, nil
	}
	res, err := f.svc.Run(context.Background(), req("build"))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Changes.Changed; len(got) != 1 || got[0] != "kept.txt" {
		t.Errorf("changed = %v, want dist/ left out", got)
	}
}

func TestRequestsThatCannotRunSayWhy(t *testing.T) {
	f := newFixture(t, Config{})
	f.put("bad", ManifestPath, "image: evil:latest\n")
	f.put("slow", ManifestPath, "timeout: soon\n")
	cases := []struct {
		name string
		edit func(*Request)
		want string
	}{
		{"no command", func(r *Request) { r.Command = " " }, "missing command"},
		{"no scope", func(r *Request) { r.Scope = "" }, "no scope"},
		{"bad project", func(r *Request) { r.Project = "a|b" }, "not a usable project name"},
		{"unknown environment", func(r *Request) { r.Environment = "rust" }, `unknown environment "rust" (declared: node, python)`},
		{"no environment", func(r *Request) { r.Profile.Environment = "" }, "no environment is configured"},
		{"missing workdir", func(r *Request) { r.WorkDir = "nope" }, `workdir "nope" does not exist`},
		{"manifest with an image", func(r *Request) { r.Project = "bad" }, "it may set environment, env, workdir, timeout and ignore"},
		{"manifest timeout", func(r *Request) { r.Project = "slow" }, "unusable timeout"},
		{"env name", func(r *Request) { r.Env = map[string]string{"A=B": "1"} }, "not a usable environment variable name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := req("true")
			tc.edit(&r)
			_, err := f.svc.Run(context.Background(), r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}

	f.runner.unavailable = fmt.Errorf("%w: no runsc", sandbox.ErrUnavailable)
	if _, err := f.svc.Run(context.Background(), req("true")); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Errorf("an unavailable sandbox gave %v", err)
	}
}

func TestAFailedSandboxStillKeepsWhatTheCommandWrote(t *testing.T) {
	f := newFixture(t, Config{})
	f.runner.run = func(_ context.Context, spec sandbox.Spec) (*sandbox.Result, error) {
		_ = os.WriteFile(filepath.Join(spec.Workspace, "partial.txt"), []byte("half"), 0o644)
		return nil, errors.New("sandbox failed: killed")
	}
	_, err := f.svc.Run(context.Background(), req("build"))
	if err == nil || !strings.Contains(err.Error(), "1 file(s) it had already changed were kept") {
		t.Fatalf("Run = %v", err)
	}
	if got := f.stored("site", "partial.txt"); got != "half" {
		t.Errorf("partial.txt in the store = %q", got)
	}
}

func TestOneJobPerProjectAndABoundedNumberOverall(t *testing.T) {
	f := newFixture(t, Config{Concurrency: 2, PerScope: 2})
	var perProject sync.Map
	var overlap atomic.Bool
	f.runner.run = func(_ context.Context, spec sandbox.Spec) (*sandbox.Result, error) {
		if _, busy := perProject.LoadOrStore(spec.Workspace, true); busy {
			overlap.Store(true)
		}
		time.Sleep(30 * time.Millisecond)
		perProject.Delete(spec.Workspace)
		return &sandbox.Result{}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := req("true")
			r.Project = fmt.Sprintf("p%d", i%3)
			if _, err := f.svc.Run(context.Background(), r); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if overlap.Load() {
		t.Error("two jobs ran in one project's workspace at the same time")
	}
	if peak := f.runner.peak.Load(); peak != 2 {
		t.Errorf("peak concurrency = %d, want exactly the configured 2", peak)
	}
}

func TestAJobThatWaitsTooLongSaysWhatItWaitedFor(t *testing.T) {
	f := newFixture(t, Config{QueueTimeout: 50 * time.Millisecond})
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	f.runner.run = func(context.Context, sandbox.Spec) (*sandbox.Result, error) {
		started <- struct{}{}
		<-release
		return &sandbox.Result{}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Run(context.Background(), req("long"))
		done <- err
	}()
	<-started
	_, err := f.svc.Run(context.Background(), req("second"))
	if err == nil || !strings.Contains(err.Error(), `another command on project "site" to finish`) {
		t.Errorf("Run = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The slot is free again.
	f.runner.run = nil
	if _, err := f.svc.Run(context.Background(), req("third")); err != nil {
		t.Errorf("the queue did not recover: %v", err)
	}
}

func TestBackgroundJobReportsWhileRunningAndWhenDone(t *testing.T) {
	f := newFixture(t, Config{})
	f.svc.SetStore(context.Background(), f.rows, "inst-a")
	wrote := make(chan struct{})
	release := make(chan struct{})
	f.runner.run = func(_ context.Context, spec sandbox.Spec) (*sandbox.Result, error) {
		spec.OnOutput("stdout", []byte("compiling…\n"))
		spec.OnOutput("stderr", []byte("warning\n"))
		close(wrote)
		<-release
		_ = os.WriteFile(filepath.Join(spec.Workspace, "bin"), []byte("x"), 0o755)
		return &sandbox.Result{ExitCode: 0, Duration: 2 * time.Second}, nil
	}
	finished := make(chan Job, 1)
	job, err := f.svc.Start(context.Background(), req("make all"), func(j Job) { finished <- j })
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobRunning || job.ID == "" || job.Instance != "inst-a" {
		t.Fatalf("started job = %+v", job)
	}
	if spec := waitSpec(t, f.runner); spec.Timeout != 2*time.Hour {
		t.Errorf("background timeout = %s, want the background ceiling", spec.Timeout)
	}
	<-wrote
	if f.svc.Running("agent|s1") != 1 || f.svc.Running("agent|other") != 0 {
		t.Error("Running does not count the session's job")
	}
	mid, err := f.svc.Status(context.Background(), "agent|s1", job.ID)
	if err != nil || mid.State != JobRunning || mid.Stdout != "compiling…\n" || mid.Stderr != "warning\n" {
		t.Errorf("status while running = %+v, %v", mid, err)
	}
	if _, err := f.svc.Status(context.Background(), "agent|other", job.ID); !errors.Is(err, errNoJob) {
		t.Errorf("another session could read the job: %v", err)
	}

	close(release)
	final := <-finished
	if final.State != JobFinished || final.DurationMS != 2000 || final.Changes == nil || len(final.Changes.Changed) != 1 {
		t.Errorf("final job = %+v", final)
	}
	// From the record now: the job is no longer live.
	after, err := f.svc.Status(context.Background(), "agent|s1", job.ID)
	if err != nil || after.State != JobFinished || after.Stdout != "compiling…\n" {
		t.Errorf("status after the end = %+v, %v", after, err)
	}
	if f.svc.Running("agent|s1") != 0 {
		t.Error("a finished job still counts as running")
	}
}

func waitSpec(t *testing.T, r *fakeRunner) sandbox.Spec {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		n := len(r.specs)
		r.mu.Unlock()
		if n > 0 {
			return r.last(t)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the job never reached the runner")
	return sandbox.Spec{}
}

func TestBackgroundJobCanBeCancelledAndEndsWithItsSession(t *testing.T) {
	f := newFixture(t, Config{})
	f.runner.run = func(ctx context.Context, _ sandbox.Spec) (*sandbox.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	finished := make(chan Job, 2)
	first, err := f.svc.Start(context.Background(), req("sleep"), func(j Job) { finished <- j })
	if err != nil {
		t.Fatal(err)
	}
	waitSpec(t, f.runner)
	got, err := f.svc.Cancel(context.Background(), "agent|s1", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != JobCancelled {
		t.Errorf("Cancel returned %+v, want the cancelled job", got)
	}
	if j := <-finished; j.State != JobCancelled || j.ID != first.ID {
		t.Errorf("cancelled job = %+v", j)
	}

	r := req("sleep")
	r.Project = "other"
	if _, err := f.svc.Start(context.Background(), r, func(j Job) { finished <- j }); err != nil {
		t.Fatal(err)
	}
	f.svc.CancelOwner("agent|s1")
	select {
	case j := <-finished:
		if j.State != JobCancelled {
			t.Errorf("job of an ended session = %+v", j)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a job outlived its session")
	}
}

func TestBackgroundJobRefusesAMistakeUpFrontAndBoundsASession(t *testing.T) {
	f := newFixture(t, Config{})
	r := req("true")
	r.Environment = "rust"
	if _, err := f.svc.Start(context.Background(), r, nil); err == nil || !strings.Contains(err.Error(), "unknown environment") {
		t.Errorf("Start = %v, want the mistake reported at once", err)
	}
	block := make(chan struct{})
	defer close(block)
	f.runner.run = func(context.Context, sandbox.Spec) (*sandbox.Result, error) {
		<-block
		return &sandbox.Result{}, nil
	}
	for i := 0; i < maxPerOwner; i++ {
		r := req("true")
		r.Project = fmt.Sprintf("p%d", i)
		if _, err := f.svc.Start(context.Background(), r, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.Start(context.Background(), req("true"), nil); err == nil || !strings.Contains(err.Error(), "background jobs running") {
		t.Errorf("Start past the limit = %v", err)
	}
}

func TestAJobWhoseInstanceDiedIsReportedLost(t *testing.T) {
	f := newFixture(t, Config{})
	f.svc.SetStore(context.Background(), f.rows, "inst-b")
	stale := Job{ID: "jdead", Owner: "agent|s1", State: JobRunning, Instance: "inst-a",
		StartedAt: time.Now().Add(-time.Hour).Unix(), Beat: time.Now().Add(-10 * time.Minute).Unix()}
	fresh := stale
	fresh.ID, fresh.Beat = "jalive", time.Now().Unix()
	for _, j := range []Job{stale, fresh} {
		raw, _ := json.Marshal(j)
		if err := f.rows.PutRow(context.Background(), jobRowPrefix+j.ID, string(raw), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.svc.Status(context.Background(), "agent|s1", "jdead")
	if err != nil || got.State != JobFailed || !strings.Contains(got.Error, "was lost") {
		t.Errorf("status of a job on a dead instance = %+v, %v", got, err)
	}
	got, err = f.svc.Status(context.Background(), "agent|s1", "jalive")
	if err != nil || got.State != JobRunning {
		t.Errorf("status of a job on a live peer = %+v, %v", got, err)
	}
	if _, err := f.svc.Cancel(context.Background(), "agent|s1", "jalive"); err == nil || !strings.Contains(err.Error(), "another instance (inst-a)") {
		t.Errorf("cancelling a peer's job gave %v", err)
	}
	if _, err := f.svc.Status(context.Background(), "agent|s1", "jnever"); !errors.Is(err, errNoJob) {
		t.Errorf("an unknown id gave %v", err)
	}
}

func TestTailKeepsTheEndAndStaysText(t *testing.T) {
	var tl tail
	tl.write([]byte(strings.Repeat("a", tailBytes)))
	tl.write([]byte("é-end"))
	got := tl.String()
	if len(got) > tailBytes || !strings.HasSuffix(got, "é-end") {
		t.Errorf("tail = …%q (%d bytes)", got[len(got)-10:], len(got))
	}
	tl = tail{}
	tl.write([]byte("x"))
	tl.write([]byte(strings.Repeat("€", tailBytes))) // cuts inside a character
	if got := tl.String(); strings.ContainsRune(got, '�') && !strings.HasPrefix(got, "€") {
		t.Errorf("tail starts with a broken character: %q", got[:8])
	}
}
