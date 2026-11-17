package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Runsc runs jobs with gVisor's runsc, called directly: no container daemon.
//
// Every job is `runsc run` on a bundle written for it, and the launcher is a
// child of this process, so the job's lifetime is at most this process's.
// Without root the sandbox runs in a user namespace (--rootless), which the
// spike behind this package showed is enough for a job: a read-only root, a
// bind-mounted workspace and tmpfs scratch all work, and start-up is tens of
// milliseconds.
//
// What rootless cannot do is cgroups, so memory and CPU limits are applied
// from outside instead: when the host has a systemd manager that will create
// a scope for us, the launcher starts inside one and the kernel enforces the
// limits on the whole sandbox. Where it does not, the limits are not enforced
// and Limiting says so; the process count, tmpfs sizes, the time limit and the
// output cap hold either way.
type Runsc struct {
	bin   string // absolute path to runsc
	dir   string // data directory: bundles and runsc's own state
	log   *slog.Logger
	scope []string // systemd-run argv prefix, nil when limits cannot be enforced

	availErr  error  // why jobs cannot run, decided once
	version   string // runsc --version, first line
	scopeNote string // why there is no scope, for doctor
	once      sync.Once
}

// NewRunsc prepares a runner. bin may be empty (look on PATH) and dir is
// created on first use. Nothing is probed until the runner is first asked
// about: a deployment with no shell profile never executes runsc at all.
func NewRunsc(bin, dir string, log *slog.Logger) *Runsc {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Runsc{bin: bin, dir: dir, log: log.With("module", "sandbox")}
}

// Available reports why jobs cannot run here, or nil.
func (r *Runsc) Available() error {
	r.once.Do(r.probe)
	return r.availErr
}

// Version is runsc's own version line, empty when it is not available.
func (r *Runsc) Version() string {
	r.once.Do(r.probe)
	return r.version
}

// Limiting describes whether memory and CPU limits are enforced, in a
// sentence an operator can act on.
func (r *Runsc) Limiting() (enforced bool, note string) {
	r.once.Do(r.probe)
	if r.scope != nil {
		return true, "memory and CPU limits are enforced through a systemd scope"
	}
	return false, "memory and CPU limits are not enforced: " + r.scopeNote
}

func (r *Runsc) probe() {
	if runtime.GOOS != "linux" {
		r.availErr = fmt.Errorf("%w: gVisor runs on Linux only, and this is %s", ErrUnavailable, runtime.GOOS)
		return
	}
	bin := r.bin
	if bin == "" {
		bin = "runsc"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		r.availErr = fmt.Errorf("%w: runsc was not found (%s): install gVisor and put runsc on PATH, or set runtime.sandbox.runsc", ErrUnavailable, bin)
		return
	}
	if path, err = filepath.Abs(path); err != nil {
		r.availErr = fmt.Errorf("%w: %v", ErrUnavailable, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		r.availErr = fmt.Errorf("%w: %s --version failed: %v", ErrUnavailable, path, err)
		return
	}
	r.bin = path
	r.version, _, _ = strings.Cut(strings.TrimSpace(string(out)), "\n")
	if r.dir, err = filepath.Abs(r.dir); err != nil {
		r.availErr = fmt.Errorf("%w: %v", ErrUnavailable, err)
		return
	}
	for _, d := range []string{r.stateDir(), r.jobsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			r.availErr = fmt.Errorf("%w: %v", ErrUnavailable, err)
			return
		}
	}
	r.reclaim(ctx)
	r.scope, r.scopeNote = probeScope(ctx)
	r.log.Info("sandbox ready", "runsc", r.bin, "version", r.version, "rootless", os.Geteuid() != 0, "limits_enforced", r.scope != nil)
}

// probeScope finds out whether a systemd manager will run a command for us
// inside a transient scope with resource limits. It is asked rather than
// assumed: a container, a minimal init or a user session without a manager all
// have systemd-run on disk and no manager behind it.
func probeScope(ctx context.Context) ([]string, string) {
	bin, err := exec.LookPath("systemd-run")
	if err != nil {
		return nil, "systemd-run was not found; run AgentFlow under systemd, or bound it from outside (a container or cgroup limit)"
	}
	prefix := []string{bin}
	if os.Geteuid() != 0 {
		prefix = append(prefix, "--user")
	}
	prefix = append(prefix, "--scope", "--quiet", "--collect")
	trial := append(append([]string{}, prefix[1:]...), "-p", "MemoryMax=64M", "--", "true")
	if out, err := exec.CommandContext(ctx, bin, trial...).CombinedOutput(); err != nil {
		return nil, fmt.Sprintf("systemd would not create a scope (%s); run AgentFlow as a systemd service or in a login session", firstLine(string(out), err))
	}
	return prefix, ""
}

func firstLine(out string, err error) string {
	if line, _, _ := strings.Cut(strings.TrimSpace(out), "\n"); line != "" {
		return line
	}
	return err.Error()
}

func (r *Runsc) stateDir() string { return filepath.Join(r.dir, "state") }
func (r *Runsc) jobsDir() string  { return filepath.Join(r.dir, "jobs") }

// global are the flags every runsc invocation takes.
func (r *Runsc) global() []string {
	args := []string{"--root=" + r.stateDir()}
	if os.Geteuid() != 0 {
		args = append(args, "--rootless")
	}
	return args
}

// reclaim removes what a previous process left in this data directory: state
// records of sandboxes that died with it, and their bundles. The sandboxes
// themselves are gone already, since they cannot outlive their launcher.
func (r *Runsc) reclaim(ctx context.Context) {
	out, err := exec.CommandContext(ctx, r.bin, append(r.global(), "list", "-quiet")...).Output()
	if err == nil {
		for _, id := range strings.Fields(string(out)) {
			r.remove(ctx, id)
		}
	}
	entries, _ := os.ReadDir(r.jobsDir())
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(r.jobsDir(), e.Name()))
	}
}

// remove deletes a sandbox's state record. A record that is already gone is
// the outcome wanted.
func (r *Runsc) remove(ctx context.Context, id string) {
	out, err := exec.CommandContext(ctx, r.bin, append(r.global(), "delete", "--force", id)...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "does not exist") {
		r.log.Warn("sandbox state not removed", "job", id, "err", firstLine(string(out), err))
	}
}

// Run executes one job.
func (r *Runsc) Run(ctx context.Context, spec Spec) (*Result, error) {
	if err := r.Available(); err != nil {
		return nil, err
	}
	if err := spec.validate(); err != nil {
		return nil, err
	}
	bundle := filepath.Join(r.jobsDir(), spec.ID)
	if err := writeBundle(bundle, spec); err != nil {
		return nil, fmt.Errorf("sandbox: write bundle: %w", err)
	}
	defer func() {
		// Its own deadline: the job's context is spent by now if it timed out.
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		r.remove(cleanup, spec.ID)
		_ = os.RemoveAll(bundle)
	}()

	runCtx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	logFile := filepath.Join(bundle, "runsc.log")
	args := append(r.global(), "--network=none", "--log="+logFile, "--log-format=json",
		"run", "--bundle="+bundle, spec.ID)
	argv := append([]string{r.bin}, args...)
	if r.scope != nil {
		argv = append(append(append([]string{}, r.scope...), scopeLimits(spec.Limits)...), append([]string{"--"}, argv...)...)
	}

	stdout := &capped{limit: spec.Limits.outputBytes()}
	stderr := &capped{limit: spec.Limits.outputBytes()}
	if spec.OnOutput != nil {
		stdout.tap = func(p []byte) { spec.OnOutput("stdout", p) }
		stderr.tap = func(p []byte) { spec.OnOutput("stderr", p) }
	}
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Killing the launcher takes the sandbox with it; the delay bounds how
	// long a stuck pipe may hold Wait after that.
	cmd.WaitDelay = 5 * time.Second
	tieToParent(cmd)

	start := time.Now()
	err := runLocked(cmd)
	res := &Result{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Duration:  time.Since(start),
		Truncated: stdout.wasTruncated() || stderr.wasTruncated(),
	}
	if ctx.Err() != nil {
		// The caller gave up (turn cancelled, session stopped): there is no
		// one to hand a result to.
		return nil, ctx.Err()
	}
	if runCtx.Err() != nil {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	if msg := launcherError(logFile); msg != "" {
		// runsc writes its own failures here and nowhere else, which is what
		// tells "the sandbox did not work" from "the command exited 128".
		return nil, fmt.Errorf("sandbox failed: %s", msg)
	}
	if r.diedUnderneath(spec.ID) {
		// A job that ends on its own has its record removed by `runsc run`. One
		// that is still on record was killed from outside the sandbox, and the
		// only thing here that does that is the kernel enforcing a limit.
		msg := "the sandbox was killed before the command finished"
		if r.scope != nil && spec.Limits.MemoryBytes > 0 {
			msg += fmt.Sprintf(": it most likely exceeded its memory limit of %d MiB", spec.Limits.MemoryBytes>>20)
		}
		return nil, errors.New("sandbox failed: " + msg)
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitCode()
	default:
		return nil, fmt.Errorf("%w: start runsc: %v", ErrUnavailable, err)
	}
	return res, nil
}

// diedUnderneath reports whether a finished job's sandbox is still on record.
func (r *Runsc) diedUnderneath(id string) bool {
	left, _ := filepath.Glob(filepath.Join(r.stateDir(), id+"_sandbox:*.state"))
	return len(left) > 0
}

// runLocked starts and waits for a command on one OS thread. The parent-death
// signal set by tieToParent is tied to the thread that forked the child, not
// to the process: without the lock a child could be killed because the Go
// runtime retired the thread it happened to be started from.
func runLocked(cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := cmd.Start(); err != nil {
			done <- err
			return
		}
		done <- cmd.Wait()
	}()
	return <-done
}

// scopeLimits are the systemd properties for a job's limits.
func scopeLimits(l Limits) []string {
	var out []string
	if l.MemoryBytes > 0 {
		// No swap: a job that swaps instead of failing turns a memory limit
		// into a slow host.
		out = append(out, "-p", fmt.Sprintf("MemoryMax=%d", l.MemoryBytes), "-p", "MemorySwapMax=0")
	}
	if l.CPUs > 0 {
		out = append(out, "-p", fmt.Sprintf("CPUQuota=%d%%", int(l.CPUs*100+0.5)))
	}
	return out
}

// launcherError returns the first error runsc logged for a job, or "".
func launcherError(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var line struct {
			Msg   string `json:"msg"`
			Level string `json:"level"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Level == "error" && line.Msg != "" {
			return line.Msg
		}
	}
	return ""
}
