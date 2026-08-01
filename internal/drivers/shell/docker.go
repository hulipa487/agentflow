package shell

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/google/uuid"
)

// DockerProvider runs each command in a brand-new, throwaway Docker
// container. It is a ONE-SHOT shell: no filesystem, environment, or process
// state carries between Exec/Read/Write calls. Every call is a self-contained
// `docker run --rm <image> sh -c '<cmd>'`, so the container lives exactly for
// that command and is removed on exit.
//
// Spawn does not start a container — it just records the image and resource
// limits on the handle, which are applied to each per-command `docker run`.
// Destroy is a no-op because containers already remove themselves.
type DockerProvider struct {
	log *slog.Logger
}

// NewDockerProvider creates the Docker shell provider.
func NewDockerProvider(log *slog.Logger) *DockerProvider {
	return &DockerProvider{log: log.With("provider", "docker")}
}

// defaultImage is used when neither the profile nor a per-spawn ShellOpts
// override supplies an image. Alpine keeps each one-shot container small and
// fast to start.
const defaultImage = "alpine:3.20"

func (p *DockerProvider) Name() string { return "docker" }

// Spawn records the one-shot environment on the handle. No container is
// created here; each Exec/Read/Write runs its own `docker run --rm`.
func (p *DockerProvider) Spawn(ctx context.Context, opts SpawnOpts) (*Handle, error) {
	image := opts.Image
	if image == "" {
		image = stringOpt(opts.ShellOpts, "image")
	}
	if image == "" {
		image = defaultImage
	}
	h := &Handle{
		ID:    "docker-" + uuid.New().String()[:8],
		State: HandleRunning,
		Image: image,
		Meta: map[string]any{
			"image":    image,
			"workdir":  opts.WorkDir,
			"network":  opts.Network,
			"mem":      opts.MemLimit,
			"cpu":      opts.CPULimit,
			"env":      opts.Env,
			"shellopts": opts.ShellOpts,
		},
		internal: nil, // no persistent container
	}
	return h, nil
}

// runArgs builds the `docker run --rm` argument vector common to Exec/Read/Write
// from the spawn-time options recorded on the handle.
func (p *DockerProvider) runArgs(handle *Handle) (image string, baseArgs []string) {
	image = handle.Image
	network := optString(handle.Meta, "network")
	if network == "" {
		network = "none"
	}
	args := []string{"run", "--rm", "--network", network}
	if mem := optString(handle.Meta, "mem"); mem != "" {
		args = append(args, "--memory", mem)
	}
	if cpu := optFloat(handle.Meta, "cpu"); cpu > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%.1f", cpu))
	}
	if wd := optString(handle.Meta, "workdir"); wd != "" {
		args = append(args, "-w", wd)
	}
	if env, ok := handle.Meta["env"].(map[string]string); ok {
		for k, v := range env {
			args = append(args, "-e", k+"="+v)
		}
	}
	return image, args
}

func (p *DockerProvider) Exec(ctx context.Context, handle *Handle, cmdStr string) (*ExecResult, error) {
	image, args := p.runArgs(handle)
	args = append(args, image, "sh", "-c", cmdStr)

	start := time.Now()
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("docker run %s: %w\nstderr: %s", image, err, stderr.String())
		}
	}
	return &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Duration: time.Since(start).Milliseconds(),
	}, nil
}

func (p *DockerProvider) Read(ctx context.Context, handle *Handle, path string) ([]byte, error) {
	image, args := p.runArgs(handle)
	args = append(args, image, "sh", "-c", "cat "+shellQuote(path))

	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker read %s: %w\nstderr: %s", path, err, stderr.String())
	}
	return out, nil
}

func (p *DockerProvider) Write(ctx context.Context, handle *Handle, path string, content []byte) error {
	image, args := p.runArgs(handle)
	// A one-shot container has no prior filesystem, so writing to a path is
	// only observable by a read within the same command. We write into a
	// parent directory if one is named; otherwise the fresh container's root.
	args = append(args, "-i", image, "sh", "-c", "cat > "+shellQuote(path))

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = bytes.NewReader(content)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker write %s: %w\nstderr: %s", path, err, stderr.String())
	}
	return nil
}

// Destroy is a no-op: one-shot containers remove themselves on exit (--rm).
func (p *DockerProvider) Destroy(ctx context.Context, handle *Handle) error {
	handle.State = HandleDestroyed
	return nil
}

// Alive reports whether the handle is still considered live. A one-shot handle
// has no long-lived container to inspect; it is "alive" while the session
// holds it.
func (p *DockerProvider) Alive(handle *Handle) bool {
	return handle.State == HandleRunning
}

// stringOpt reads a string value from the ShellOpts escape hatch.
func stringOpt(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// optString reads a string value from the handle's Meta map.
func optString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// optFloat reads a float64 value from the handle's Meta map.
func optFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
