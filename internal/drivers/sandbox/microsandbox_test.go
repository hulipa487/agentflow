package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildMicrosandboxArgs(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(filepath.Join(ws, "app"), 0o755); err != nil {
		t.Fatal(err)
	}

	spec := Spec{
		ID:        "job-42",
		Command:   "pytest -q",
		Image:     "python:3.12-slim@sha256:" + strings.Repeat("a", 64),
		Workspace: ws,
		WorkDir:   "app",
		Env:       map[string]string{"CI": "1", "PATH": "/custom"},
		Limits:    Limits{MemoryBytes: 300 << 20, CPUs: 1.5},
		Binds:     []Bind{{Source: "/srv/ro", Dest: "/ro", ReadOnly: true}, {Source: "/srv/rw", Dest: "/rw"}},
		Network:   "none",
	}
	args := buildMicrosandboxArgs(spec)
	joined := strings.Join(args, "\n")

	for _, want := range []string{
		"run",
		"--memory\n300M",
		"--cpus\n2",
		"--no-net",
		"--mount-dir\n" + ws + ":/workspace:rw",
		"--mount-dir\n/srv/ro:/ro:ro",
		"--mount-dir\n/srv/rw:/rw:rw",
		"--workdir\n/workspace/app",
		"--env\nCI=1",
		"--env\nHOME=/tmp",
		"--env\nPATH=/custom",
		spec.Image,
		"--\n/bin/sh\n-c\npytest -q",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q in:\n%s", want, joined)
		}
	}

	// Memory < 128 MiB clamps to 128M, custom rootfs emits --rootfs instead of image.
	rootSpec := Spec{
		ID:        "job-43",
		Command:   "echo hi",
		Rootfs:    "/srv/rootfs",
		Workspace: ws,
		Shell:     []string{"/bin/bash", "-lc"},
		Limits:    Limits{MemoryBytes: 64 << 20},
	}
	rootArgs := strings.Join(buildMicrosandboxArgs(rootSpec), "\n")
	if !strings.Contains(rootArgs, "--memory\n128M") {
		t.Errorf("expected minimum 128M memory limit, got:\n%s", rootArgs)
	}
	if !strings.Contains(rootArgs, "--rootfs\n/srv/rootfs") {
		t.Errorf("expected --rootfs flag, got:\n%s", rootArgs)
	}
	if !strings.Contains(rootArgs, "--\n/bin/bash\n-lc\necho hi") {
		t.Errorf("expected custom shell invocation, got:\n%s", rootArgs)
	}
}

func TestMicrosandboxMissingBinaryIsUnavailable(t *testing.T) {
	m := NewMicrosandbox("nonexistent-msb-binary-agentflow-test", t.TempDir(), nil)
	err := m.Available()
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Available() = %v; want ErrUnavailable", err)
	}
	if enforced, _ := m.Limiting(); enforced {
		t.Fatal("Limiting() reported enforced=true when binary is missing")
	}
}

// buildFakeMSB compiles a small Go helper binary that acts as `msb` across
// Windows, macOS, and Linux so Microsandbox.Run can be tested end-to-end.
func buildFakeMSB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := `package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("msb 0.2.6-test")
		return
	}
	if os.Getenv("AF_TEST_MODE") == "" {
		for _, a := range os.Args[1:] {
			if strings.HasPrefix(a, "AF_TEST_MODE=") {
				os.Setenv("AF_TEST_MODE", strings.TrimPrefix(a, "AF_TEST_MODE="))
			}
		}
	}
	// Detect mode from the final shell command argument.
	cmd := os.Args[len(os.Args)-1]
	switch {
	case strings.Contains(cmd, "ARGS"):
		fmt.Println(strings.Join(os.Args[1:], "|"))
		fmt.Fprintln(os.Stderr, "err-line")
	case strings.Contains(cmd, "FAIL3"):
		fmt.Fprintln(os.Stderr, "boom")
		os.Exit(3)
	case strings.Contains(cmd, "BIG"):
		fmt.Print(strings.Repeat("x", 64))
	case strings.Contains(cmd, "SLEEP"):
		time.Sleep(2 * time.Second)
	}
}
`
	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "msb")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	out, err := exec.Command("go", "build", "-o", bin, srcPath).CombinedOutput()
	if err != nil {
		t.Fatalf("build fake msb: %v\n%s", err, out)
	}
	return bin
}

func TestMicrosandboxRunSubprocess(t *testing.T) {
	fake := buildFakeMSB(t)
	dataDir := t.TempDir()
	ws := t.TempDir()

	newRunner := func() *Microsandbox {
		m := NewMicrosandbox(fake, dataDir, nil)
		m.checkHypervisor = func() error { return nil }
		return m
	}

	m := newRunner()
	if err := m.Available(); err != nil {
		t.Fatalf("Available(): %v", err)
	}
	if got := m.Version(); got != "msb 0.2.6-test" {
		t.Fatalf("Version() = %q", got)
	}
	if enforced, _ := m.Limiting(); !enforced {
		t.Fatal("Limiting() = false, want true")
	}

	img := "python:3.12-slim@sha256:" + strings.Repeat("b", 64)

	t.Run("success and tap", func(t *testing.T) {
		var mu sync.Mutex
		var streamed string
		res, err := m.Run(context.Background(), Spec{
			ID:        "job-ok",
			Command:   "ARGS",
			Image:     img,
			Workspace: ws,
			Limits:    Limits{MemoryBytes: 512 << 20, CPUs: 2},
			OnOutput: func(stream string, p []byte) {
				mu.Lock()
				streamed += stream + ":" + string(p)
				mu.Unlock()
			},
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
		}
		if !strings.Contains(res.Stdout, "--memory|512M") || !strings.Contains(res.Stdout, img) {
			t.Errorf("unexpected argv stdout: %q", res.Stdout)
		}
		if strings.TrimSpace(res.Stderr) != "err-line" {
			t.Errorf("Stderr = %q, want err-line", res.Stderr)
		}
		mu.Lock()
		gotStream := streamed
		mu.Unlock()
		if !strings.Contains(gotStream, "stdout:") || !strings.Contains(gotStream, "stderr:") {
			t.Errorf("OnOutput missed stream data: %q", gotStream)
		}
	})

	t.Run("non-zero exit", func(t *testing.T) {
		res, err := m.Run(context.Background(), Spec{
			ID:        "job-fail",
			Command:   "FAIL3",
			Image:     img,
			Workspace: ws,
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.ExitCode != 3 || strings.TrimSpace(res.Stderr) != "boom" {
			t.Fatalf("got ExitCode=%d Stderr=%q, want 3/boom", res.ExitCode, res.Stderr)
		}
	})

	t.Run("output truncation", func(t *testing.T) {
		res, err := m.Run(context.Background(), Spec{
			ID:        "job-big",
			Command:   "BIG",
			Image:     img,
			Workspace: ws,
			Limits:    Limits{OutputBytes: 16},
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !res.Truncated || len(res.Stdout) != 16 {
			t.Fatalf("got Truncated=%v len=%d, want true/16", res.Truncated, len(res.Stdout))
		}
	})

	t.Run("timeout", func(t *testing.T) {
		res, err := m.Run(context.Background(), Spec{
			ID:        "job-timeout",
			Command:   "SLEEP",
			Image:     img,
			Workspace: ws,
			Timeout:   80 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !res.TimedOut || res.ExitCode != -1 {
			t.Fatalf("got TimedOut=%v ExitCode=%d, want true/-1", res.TimedOut, res.ExitCode)
		}
	})
}

func TestNativeImagesResolveSkipsHostRootfs(t *testing.T) {
	img := "python:3.12-slim@sha256:" + strings.Repeat("c", 64)
	envs := NewEnvs(t.TempDir(), map[string]EnvSpec{
		"py": {Image: img, Env: map[string]string{"FOO": "bar"}},
	}, nil)
	envs.SetNativeImages(true)

	got, err := envs.Resolve(context.Background(), "py")
	if err != nil {
		t.Fatalf("Resolve with SetNativeImages(true) failed: %v", err)
	}
	if got.Image != img || got.Rootfs != "" || got.Env["FOO"] != "bar" {
		t.Fatalf("unexpected resolved env: %+v", got)
	}
}
