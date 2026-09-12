package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DockerProvider runs a single long-lived Docker container per shell handle.
// Spawn creates the container (`docker run -d`), and Exec/Read/Write operate
// against it with `docker exec`, so filesystem, environment, and process state
// persist across calls until Destroy tears the container down. This is a
// full persistent shell, not a one-shot throwaway.
//
// Resource limits (network, memory, cpu) and the working directory / env are
// baked into the container at Spawn; per-op calls only carry the command.
type DockerProvider struct {
	log *slog.Logger
}

// NewDockerProvider creates the Docker shell provider.
func NewDockerProvider(log *slog.Logger) *DockerProvider {
	return &DockerProvider{log: log.With("provider", "docker")}
}

// defaultImage is used when neither the profile nor a per-spawn ShellOpts
// override supplies an image. Alpine keeps the container small and fast.
const defaultImage = "alpine:3.20"

func (p *DockerProvider) Name() string { return "docker" }

// Spawn creates a detached container and parks it with `sleep infinity` so
// it stays running and ready for `docker exec`. The container ID is stored
// on the handle's internal field.
func (p *DockerProvider) Spawn(ctx context.Context, opts SpawnOpts) (*Handle, error) {
	image := opts.Image
	if image == "" {
		image = stringOpt(opts.ShellOpts, "image")
	}
	if image == "" {
		image = defaultImage
	}

	network := opts.Network
	if network == "" {
		network = "none"
	}

	args := []string{"run", "-d", "--network", network}
	if mem := opts.MemLimit; mem != "" {
		args = append(args, "--memory", mem)
	}
	if opts.CPULimit > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%.1f", opts.CPULimit))
	}
	if wd := opts.WorkDir; wd != "" {
		args = append(args, "-w", wd)
	}
	for k, v := range opts.Env {
		args = append(args, "-e", k+"="+v)
	}
	// Park the container so it stays alive between exec calls. `sleep` is
	// present on alpine and busybox-based images; fall back to tail for
	// minimal images that lack coreutils sleep.
	args = append(args, image, "sh", "-c", "sleep infinity || tail -f /dev/null")

	containerID, stderr, err := runDocker(ctx, args, nil)
	if err != nil {
		return nil, fmt.Errorf("docker run %s: %w\nstderr: %s", image, err, stderr)
	}
	containerID = strings.TrimSpace(containerID)
	if containerID == "" {
		return nil, fmt.Errorf("docker run %s: no container id returned\nstderr: %s", image, stderr)
	}

	// Confirm the container actually stayed up. A bad image or entrypoint
	// exits immediately; surface its logs rather than a silent later failure.
	if err := p.waitForRunning(ctx, containerID, image); err != nil {
		_ = p.removeContainer(context.Background(), containerID) // best-effort cleanup
		return nil, err
	}

	h := &Handle{
		ID:       "docker-" + uuid.New().String()[:8],
		Provider: "docker",
		State:    HandleRunning,
		Image:    image,
		Meta: map[string]any{
			"image":     image,
			"container": containerID,
		},
		internal: containerID,
	}
	p.log.Info("docker shell ready", "handle_id", h.ID, "container", containerID, "image", image)
	return h, nil
}

// waitForRunning polls `docker inspect` briefly to confirm the container is in
// running state. If it has already exited, returns its logs so the caller can
// see why (bad image, missing command, etc.).
func (p *DockerProvider) waitForRunning(ctx context.Context, containerID, image string) error {
	const deadline = 3 * time.Second
	start := time.Now()
	for {
		out, _, err := runDocker(ctx, []string{"inspect", "-f", "{{.State.Running}}", containerID}, nil)
		if err == nil {
			if strings.TrimSpace(out) == "true" {
				return nil
			}
		}
		if time.Since(start) >= deadline {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	// The container exited; pull its logs for a useful error.
	logs, _, _ := runDocker(ctx, []string{"logs", containerID}, nil)
	return fmt.Errorf("docker container for %s exited immediately\nlogs: %s", image, strings.TrimSpace(logs))
}

func (p *DockerProvider) Exec(ctx context.Context, handle *Handle, cmdStr string) (*ExecResult, error) {
	id, err := p.containerID(handle)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	stdout, stderr, exitCode, err := runDockerFull(ctx, []string{"exec", id, "sh", "-c", cmdStr}, nil)
	if err != nil && exitCode < 0 {
		// A non-ExitError failure (docker itself failed, ctx cancel, etc.).
		return nil, fmt.Errorf("docker exec: %w\nstderr: %s", err, stderr)
	}
	return &ExecResult{
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: exitCode,
		Duration: time.Since(start).Milliseconds(),
	}, nil
}

func (p *DockerProvider) Read(ctx context.Context, handle *Handle, path string) ([]byte, error) {
	id, err := p.containerID(handle)
	if err != nil {
		return nil, err
	}
	stdout, stderr, exitCode, err := runDockerFull(ctx, []string{"exec", id, "sh", "-c", "cat " + shellQuote(path)}, nil)
	if err != nil && exitCode < 0 {
		return nil, fmt.Errorf("docker read %s: %w\nstderr: %s", path, err, stderr)
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("docker read %s failed: %s", path, strings.TrimSpace(stderr))
	}
	return []byte(stdout), nil
}

func (p *DockerProvider) Write(ctx context.Context, handle *Handle, path string, content []byte) error {
	id, err := p.containerID(handle)
	if err != nil {
		return err
	}
	_, stderr, exitCode, err := runDockerFull(ctx, []string{"exec", "-i", id, "sh", "-c", "cat > " + shellQuote(path)}, content)
	if err != nil && exitCode < 0 {
		return fmt.Errorf("docker write %s: %w\nstderr: %s", path, err, stderr)
	}
	if exitCode != 0 {
		return fmt.Errorf("docker write %s failed: %s", path, strings.TrimSpace(stderr))
	}
	return nil
}

// Destroy force-removes the container. A container that already vanished
// (e.g. externally removed) is not an error.
func (p *DockerProvider) Destroy(ctx context.Context, handle *Handle) error {
	id, err := p.containerID(handle)
	if err != nil {
		handle.State = HandleDestroyed
		return nil
	}
	if err := p.removeContainer(ctx, id); err != nil {
		p.log.Warn("docker remove failed", "container", id, "err", err)
	}
	handle.State = HandleDestroyed
	return nil
}

// Alive reports whether the container is still running.
func (p *DockerProvider) Alive(handle *Handle) bool {
	id, ok := handle.internal.(string)
	if !ok || id == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, _, err := runDocker(ctx, []string{"inspect", "-f", "{{.State.Running}}", id}, nil)
	return err == nil && strings.TrimSpace(out) == "true"
}

// containerID returns the stored container id, or an error if the handle is
// not a live docker handle.
func (p *DockerProvider) containerID(handle *Handle) (string, error) {
	id, ok := handle.internal.(string)
	if !ok || id == "" {
		return "", fmt.Errorf("docker: handle %q has no container", handle.ID)
	}
	return id, nil
}

// removeContainer force-removes a container, ignoring "no such container".
func (p *DockerProvider) removeContainer(ctx context.Context, id string) error {
	_, _, _, err := runDockerFull(ctx, []string{"rm", "-f", id}, nil)
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

// runDocker runs `docker <args>` and returns trimmed stdout + stderr. A non-zero
// exit is returned as an error carrying stderr.
func runDocker(ctx context.Context, args []string, stdin []byte) (stdout, stderr string, err error) {
	out, errb, _, err := runDockerFull(ctx, args, stdin)
	return out, errb, err
}

// runDockerFull runs `docker <args>` and returns stdout, stderr, the exit code,
// and any non-ExitError. A non-zero exit code from docker is reported via the
// returned error (with stderr), but exitCode is still set so callers can
// distinguish a command's non-zero exit (keep the output) from a docker-level
// failure (treat as an error). exitCode is -1 when docker itself failed to run.
func runDockerFull(ctx context.Context, args []string, stdin []byte) (stdout, stderr string, exitCode int, err error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb

	exitCode = -1
	runErr := cmd.Run()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
			return out.String(), errb.String(), exitCode, runErr
		}
		return out.String(), errb.String(), -1, runErr
	}
	return out.String(), errb.String(), 0, nil
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
