package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Microsandbox runs isolated shell jobs out-of-process via the `msb` CLI from
// superradcompany/microsandbox (libkrun microVMs over KVM on Linux, Apple Silicon
// HVF on macOS, or WHP on Windows), keeping the agentflow binary 100% pure Go
// (`CGO_ENABLED=0`).
type Microsandbox struct {
	bin string // resolved path to msb binary
	dir string // sandbox data directory
	log *slog.Logger

	// checkHypervisor is called once during probe(); nil uses the OS default
	// (requiring accessible /dev/kvm on Linux; no-op on macOS/Windows).
	checkHypervisor func() error

	availErr error
	version  string
	once     sync.Once
}

// NewMicrosandbox prepares a Microsandbox runner. bin may be empty (look for
// "msb" on PATH). Nothing is probed until Available, Version, Limiting, or
// Run is called.
func NewMicrosandbox(bin, dir string, log *slog.Logger) *Microsandbox {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Microsandbox{bin: bin, dir: dir, log: log.With("module", "sandbox", "provider", "microsandbox")}
}

// Available reports why microsandbox jobs cannot run here, or nil.
func (m *Microsandbox) Available() error {
	m.once.Do(m.probe)
	return m.availErr
}

// Version returns the first line reported by `msb --version`, or empty if
// unavailable.
func (m *Microsandbox) Version() string {
	m.once.Do(m.probe)
	return m.version
}

// Limiting reports whether memory and CPU limits are enforced for jobs.
// Microsandbox passes VM resource limits directly to the hypervisor.
func (m *Microsandbox) Limiting() (enforced bool, note string) {
	m.once.Do(m.probe)
	if m.availErr != nil {
		return false, m.availErr.Error()
	}
	return true, "memory and CPU limits are enforced by the microsandbox microVM hypervisor"
}

func defaultHypervisorCheck() error {
	switch runtime.GOOS {
	case "linux":
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("/dev/kvm is not accessible (%v); KVM hardware virtualization is required for microsandbox on Linux", err)
		}
		_ = f.Close()
		return nil
	case "darwin", "windows":
		return nil
	default:
		return fmt.Errorf("microsandbox is not supported on %s", runtime.GOOS)
	}
}

func (m *Microsandbox) probe() {
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		m.availErr = fmt.Errorf("%w: microsandbox is not supported on %s", ErrUnavailable, runtime.GOOS)
		return
	}
	bin := strings.TrimSpace(m.bin)
	if bin == "" {
		bin = "msb"
	}
	p, err := exec.LookPath(bin)
	if err != nil {
		m.availErr = fmt.Errorf("%w: msb was not found (%s): install microsandbox (https://github.com/superradcompany/microsandbox) and put msb on PATH, or set runtime.sandbox.microsandbox", ErrUnavailable, bin)
		return
	}
	if p, err = filepath.Abs(p); err != nil {
		m.availErr = fmt.Errorf("%w: %v", ErrUnavailable, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, p, "--version").Output()
	if err != nil {
		m.availErr = fmt.Errorf("%w: %s --version failed: %v", ErrUnavailable, p, err)
		return
	}
	hv := m.checkHypervisor
	if hv == nil {
		hv = defaultHypervisorCheck
	}
	if err := hv(); err != nil {
		m.availErr = fmt.Errorf("%w: %v", ErrUnavailable, err)
		return
	}
	m.bin = p
	m.version, _, _ = strings.Cut(strings.TrimSpace(string(out)), "\n")
	if m.dir != "" {
		if absDir, err := filepath.Abs(m.dir); err == nil {
			m.dir = absDir
		}
		_ = os.MkdirAll(filepath.Join(m.dir, "microsandbox"), 0o700)
	}
	m.log.Info("microsandbox ready", "msb", m.bin, "version", m.version)
}

// buildMicrosandboxArgs constructs the `msb run` CLI arguments for spec.
func buildMicrosandboxArgs(spec Spec) []string {
	args := []string{"run"}

	if spec.Limits.MemoryBytes > 0 {
		mib := (spec.Limits.MemoryBytes + (1 << 20) - 1) >> 20
		if mib < 128 {
			mib = 128
		}
		args = append(args, "--memory", fmt.Sprintf("%dM", mib))
	}
	if spec.Limits.CPUs > 0 {
		cpus := int(math.Ceil(spec.Limits.CPUs))
		if cpus < 1 {
			cpus = 1
		}
		args = append(args, "--cpus", strconv.Itoa(cpus))
	}
	if spec.Network == "" || spec.Network == "none" {
		args = append(args, "--no-net")
	}

	mount := spec.mount()
	args = append(args, "--mount-dir", fmt.Sprintf("%s:%s:rw", spec.Workspace, mount))
	for _, b := range spec.Binds {
		mode := "rw"
		if b.ReadOnly {
			mode = "ro"
		}
		args = append(args, "--mount-dir", fmt.Sprintf("%s:%s:%s", b.Source, b.Dest, mode))
	}

	cwd := mount
	if spec.WorkDir != "" {
		cwd = path.Join(mount, spec.WorkDir)
	}
	args = append(args, "--workdir", cwd)

	env := make(map[string]string, len(spec.Env)+2)
	for k, v := range spec.Env {
		env[k] = v
	}
	if _, ok := env["PATH"]; !ok {
		env["PATH"] = defaultPath
	}
	if _, ok := env["HOME"]; !ok {
		env["HOME"] = "/tmp"
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+env[k])
	}

	if img := strings.TrimSpace(spec.Image); img != "" {
		args = append(args, img)
	} else {
		args = append(args, "--rootfs", spec.Rootfs)
	}

	shell := spec.Shell
	if len(shell) == 0 {
		shell = []string{"/bin/sh", "-c"}
	}
	args = append(args, "--")
	args = append(args, shell...)
	args = append(args, spec.Command)
	return args
}

// Run executes one command inside an ephemeral microsandbox microVM and
// returns its captured stdout/stderr, exit code, duration, and timeout state.
func (m *Microsandbox) Run(ctx context.Context, spec Spec) (*Result, error) {
	if err := m.Available(); err != nil {
		return nil, err
	}
	if err := spec.validateMicrosandbox(); err != nil {
		return nil, err
	}

	runCtx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	stdout := &capped{limit: spec.Limits.outputBytes()}
	stderr := &capped{limit: spec.Limits.outputBytes()}
	if spec.OnOutput != nil {
		stdout.tap = func(p []byte) { spec.OnOutput("stdout", p) }
		stderr.tap = func(p []byte) { spec.OnOutput("stderr", p) }
	}

	args := buildMicrosandboxArgs(spec)
	cmd := exec.CommandContext(runCtx, m.bin, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
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
		return nil, ctx.Err()
	}
	if runCtx.Err() != nil {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitCode()
	default:
		return nil, fmt.Errorf("%w: start msb: %v", ErrUnavailable, err)
	}
	return res, nil
}
