package shell

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DockerProvider launches and manages Docker containers via the docker CLI.
// Each container runs "sleep infinity" as its entrypoint so it stays alive
// for repeated exec calls. Tests replace docker calls with a test double.
type DockerProvider struct {
	log *slog.Logger
}

// NewDockerProvider creates the Docker shell provider.
func NewDockerProvider(log *slog.Logger) *DockerProvider {
	return &DockerProvider{log: log.With("provider", "docker")}
}

func (p *DockerProvider) Name() string { return "docker" }

func (p *DockerProvider) Spawn(ctx context.Context, opts SpawnOpts) (*Handle, error) {
	if opts.Image == "" {
		return nil, fmt.Errorf("docker: image is required")
	}

	containerName := "agentflow-" + uuid.New().String()[:8]
	network := opts.Network
	if network == "" {
		network = "none"
	}

	args := []string{
		"run", "-d",
		"--name", containerName,
		"--network", network,
	}
	if opts.MemLimit != "" {
		args = append(args, "--memory", opts.MemLimit)
	}
	if opts.CPULimit > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%.1f", opts.CPULimit))
	}
	if opts.WorkDir != "" {
		args = append(args, "-w", opts.WorkDir)
	}
	for k, v := range opts.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, opts.Image, "sleep", "infinity")

	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker spawn %s: %w\nstderr: %s", opts.Image, err, stderr.String())
	}

	containerID := strings.TrimSpace(string(out))
	h := &Handle{
		ID:       containerName,
		State:    HandleRunning,
		Image:    opts.Image,
		internal: containerID,
	}
	return h, nil
}

func (p *DockerProvider) Exec(ctx context.Context, handle *Handle, cmdStr string) (*ExecResult, error) {
	containerID := handle.internal.(string)
	start := time.Now()

	cmd := exec.CommandContext(ctx, "docker", "exec", containerID, "sh", "-c", cmdStr)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("docker exec %s: %w", containerID, err)
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
	containerID := handle.internal.(string)
	cmd := exec.CommandContext(ctx, "docker", "exec", containerID, "sh", "-c", "cat "+shellQuote(path))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker read %s: %w\nstderr: %s", path, err, stderr.String())
	}
	return out, nil
}

func (p *DockerProvider) Write(ctx context.Context, handle *Handle, path string, content []byte) error {
	containerID := handle.internal.(string)

	// Use docker exec with stdin to write a file.
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerID,
		"sh", "-c", "cat > "+shellQuote(path))
	cmd.Stdin = bytes.NewReader(content)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker write %s: %w\nstderr: %s", path, err, stderr.String())
	}
	return nil
}

func (p *DockerProvider) Destroy(ctx context.Context, handle *Handle) error {
	containerID := handle.internal.(string)

	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", containerID)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker rm %s: %w\nstderr: %s", containerID, err, stderr.String())
	}
	handle.State = HandleDestroyed
	return nil
}

func (p *DockerProvider) Alive(handle *Handle) bool {
	containerID := handle.internal.(string)
	cmd := exec.Command("docker", "inspect", containerID, "--format", "{{.State.Running}}")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}
