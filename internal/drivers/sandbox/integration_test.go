package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run real sandboxes. They need runsc (on PATH, or named by
// AGENTFLOW_RUNSC) and a root filesystem with /bin/sh: AGENTFLOW_SANDBOX_ROOTFS
// names one, and without it the host's own root is used, read-only. They skip
// where either is missing, like the other drivers' integration tests.

func realRunner(t *testing.T) (*Runsc, string) {
	t.Helper()
	r := NewRunsc(os.Getenv("AGENTFLOW_RUNSC"), t.TempDir(), nil)
	if err := r.Available(); err != nil {
		t.Skipf("no sandbox here: %v", err)
	}
	rootfs := os.Getenv("AGENTFLOW_SANDBOX_ROOTFS")
	if rootfs == "" {
		rootfs = "/"
	}
	// Lstat: in a real image /bin/sh is often a link that only resolves inside it.
	if _, err := os.Lstat(filepath.Join(rootfs, "bin", "sh")); err != nil {
		t.Skipf("no /bin/sh in the root filesystem %s", rootfs)
	}
	return r, rootfs
}

func job(t *testing.T, rootfs, command string) Spec {
	t.Helper()
	// Not t.Name(): a sandbox id must be a plain file name.
	id := "t" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return Spec{ID: id, Command: command, Rootfs: rootfs, Workspace: t.TempDir(), Timeout: 30 * time.Second}
}

func TestRunReportsOutputAndExitCode(t *testing.T) {
	r, rootfs := realRunner(t)
	spec := job(t, rootfs, `echo out; echo err >&2; echo "$GREETING"; pwd; exit 7`)
	spec.Env = map[string]string{"GREETING": "hello"}
	res, err := r.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 7 || res.TimedOut || res.Truncated {
		t.Errorf("result = %+v, want exit 7 and nothing else", res)
	}
	if res.Stdout != "out\nhello\n/workspace\n" || res.Stderr != "err\n" {
		t.Errorf("stdout = %q, stderr = %q", res.Stdout, res.Stderr)
	}
}

func TestRunSeesTheWorkspaceAndNothingElseIsWritable(t *testing.T) {
	r, rootfs := realRunner(t)
	spec := job(t, rootfs, `cat in.txt && echo made > sub/out.txt && (touch /etc/agentflow-probe 2>/dev/null && echo ROOT-WRITABLE; true) && echo scratch > /tmp/x && cat /tmp/x`)
	if err := os.WriteFile(filepath.Join(spec.Workspace, "in.txt"), []byte("given\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(spec.Workspace, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout != "given\nscratch\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", res.ExitCode, res.Stdout, res.Stderr)
	}
	got, err := os.ReadFile(filepath.Join(spec.Workspace, "sub", "out.txt"))
	if err != nil || string(got) != "made\n" {
		t.Errorf("the command's file did not reach the host workspace: %q, %v", got, err)
	}
	if _, err := os.Stat("/etc/agentflow-probe"); err == nil {
		t.Error("the command wrote to the host's root filesystem")
	}
}

func TestRunStartsInTheWorkdir(t *testing.T) {
	r, rootfs := realRunner(t)
	spec := job(t, rootfs, "pwd")
	spec.Mount = "/work"
	spec.WorkDir = "app/src"
	if err := os.MkdirAll(filepath.Join(spec.Workspace, "app", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "/work/app/src\n" {
		t.Errorf("pwd = %q", res.Stdout)
	}
}

func TestRunKillsAtTheTimeLimitAndLeavesNothing(t *testing.T) {
	r, rootfs := realRunner(t)
	spec := job(t, rootfs, "echo started; sleep 600 & sleep 600")
	spec.Timeout = 2 * time.Second
	start := time.Now()
	res, err := r.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.Stdout != "started\n" {
		t.Errorf("result = %+v, want a timeout that kept the output so far", res)
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Errorf("a 2s limit took %s to enforce", took)
	}
	assertNothingLeft(t, r, spec.ID)
}

func TestRunStopsWhenTheCallerGivesUp(t *testing.T) {
	r, rootfs := realRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(time.Second, cancel)
	spec := job(t, rootfs, "sleep 600")
	_, err := r.Run(ctx, spec)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	assertNothingLeft(t, r, spec.ID)
}

func TestRunCapsAFloodOfOutput(t *testing.T) {
	r, rootfs := realRunner(t)
	spec := job(t, rootfs, "head -c 5000000 /dev/zero | tr '\\0' x; echo done >&2")
	spec.Limits.OutputBytes = 1000
	res, err := r.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != 1000 || !res.Truncated {
		t.Errorf("kept %d bytes, truncated=%v; want exactly the cap", len(res.Stdout), res.Truncated)
	}
	if res.ExitCode != 0 || res.Stderr != "done\n" {
		t.Errorf("the flood stopped the command: exit %d, stderr %q", res.ExitCode, res.Stderr)
	}
}

func TestRunLimitsProcesses(t *testing.T) {
	r, rootfs := realRunner(t)
	spec := job(t, rootfs, `for i in $(seq 1 40); do (sleep 5 &) 2>/dev/null || { echo refused; break; }; done; echo end`)
	spec.Limits.PIDs = 8
	res, err := r.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout+res.Stderr, "refused") && !strings.Contains(strings.ToLower(res.Stderr), "fork") {
		t.Errorf("40 processes started under a limit of 8: stdout %q, stderr %q", res.Stdout, res.Stderr)
	}
}

func TestRunReportsALauncherFailureAsAnError(t *testing.T) {
	r, rootfs := realRunner(t)
	spec := job(t, rootfs, "true")
	spec.Shell = []string{"/bin/no-such-shell", "-c"}
	res, err := r.Run(context.Background(), spec)
	if err == nil {
		t.Fatalf("a missing shell came back as a result: %+v", res)
	}
	if !strings.Contains(err.Error(), "no-such-shell") {
		t.Errorf("error %q does not name what failed", err)
	}
	assertNothingLeft(t, r, spec.ID)
}

func TestRunEnforcesTheMemoryLimitWhereItCan(t *testing.T) {
	r, rootfs := realRunner(t)
	if ok, note := r.Limiting(); !ok {
		t.Skip(note)
	}
	spec := job(t, rootfs, "tail /dev/zero; echo survived")
	spec.Limits.MemoryBytes = 200 << 20
	spec.Limits.TmpBytes = 16 << 20
	start := time.Now()
	res, err := r.Run(context.Background(), spec)
	t.Logf("after %s: result %+v, error %v", time.Since(start).Round(time.Millisecond), res, err)
	// The kernel kills the sandbox, so there is no result to return: the job
	// must end promptly with an error that says what probably happened, not
	// run into its 30s time limit with the host's memory.
	if err == nil {
		t.Fatalf("a job that allocates without bound finished normally: %+v", res)
	}
	if !strings.Contains(err.Error(), "memory limit") {
		t.Errorf("error %q does not mention the memory limit", err)
	}
	assertNothingLeft(t, r, spec.ID)
}

// assertNothingLeft checks the two things a finished job must not leave: a
// state record and a bundle.
func assertNothingLeft(t *testing.T, r *Runsc, id string) {
	t.Helper()
	out, err := exec.Command(r.bin, append(r.global(), "list", "-quiet")...).Output()
	if err != nil {
		t.Fatalf("runsc list: %v", err)
	}
	if strings.Contains(string(out), id) {
		t.Errorf("sandbox %s is still registered", id)
	}
	if _, err := os.Stat(filepath.Join(r.jobsDir(), id)); !os.IsNotExist(err) {
		t.Errorf("bundle of %s is still on disk", id)
	}
}

// TestRunInAPulledImage is the whole path with nothing faked: a real registry,
// a real image, a real sandbox. It needs the network, so it runs only when
// AGENTFLOW_TEST_IMAGE names a pinned image (for example
// alpine@sha256:…).
func TestRunInAPulledImage(t *testing.T) {
	image := os.Getenv("AGENTFLOW_TEST_IMAGE")
	if image == "" {
		t.Skip("AGENTFLOW_TEST_IMAGE is not set")
	}
	r := NewRunsc(os.Getenv("AGENTFLOW_RUNSC"), t.TempDir(), nil)
	if err := r.Available(); err != nil {
		t.Skipf("no sandbox here: %v", err)
	}
	envs := NewEnvs(t.TempDir(), map[string]EnvSpec{"img": {Image: image}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := envs.Resolve(ctx, "img")
	if err != nil {
		t.Fatal(err)
	}
	spec := job(t, env.Rootfs, "cat /etc/os-release | head -2; echo $PATH; id -u")
	spec.Env = env.Env
	res, err := r.Run(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "NAME=") {
		t.Errorf("the image did not run: %+v", res)
	}
}
