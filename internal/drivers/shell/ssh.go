package shell

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

// SSHProvider implements ShellProvider for a remote SSH connection to an
// already-running host (the host is supplied in SpawnOpts; nothing here
// provisions it). Exec/Read/Write reuse the shared sshExec/sshRead/sshWrite
// helpers below for the life of the handle.
type SSHProvider struct {
	log *slog.Logger
}

// NewSSHProvider creates the SSH shell provider.
func NewSSHProvider(log *slog.Logger) *SSHProvider {
	return &SSHProvider{log: log.With("provider", "ssh")}
}

func (p *SSHProvider) Name() string { return "ssh" }

func (p *SSHProvider) Spawn(ctx context.Context, opts SpawnOpts) (*Handle, error) {
	if opts.Host == "" {
		return nil, fmt.Errorf("ssh: host is required")
	}
	if opts.User == "" {
		return nil, fmt.Errorf("ssh: user is required")
	}

	client, err := sshDial(ctx, opts.Host, opts.User, opts)
	if err != nil {
		return nil, err
	}
	h := &Handle{
		ID:       "ssh-" + uuid.New().String()[:8],
		Provider: "ssh",
		State:    HandleRunning,
		Meta: map[string]any{
			"host": opts.Host,
			"user": opts.User,
		},
		internal: client,
	}
	return h, nil
}

func (p *SSHProvider) Exec(ctx context.Context, handle *Handle, cmdStr string) (*ExecResult, error) {
	return sshExec(ctx, handle.internal.(*ssh.Client), cmdStr)
}

func (p *SSHProvider) Read(ctx context.Context, handle *Handle, path string) ([]byte, error) {
	return sshRead(ctx, handle.internal.(*ssh.Client), path)
}

func (p *SSHProvider) Write(ctx context.Context, handle *Handle, path string, content []byte) error {
	return sshWrite(ctx, handle.internal.(*ssh.Client), path, content)
}

func (p *SSHProvider) Destroy(ctx context.Context, handle *Handle) error {
	client := handle.internal.(*ssh.Client)
	err := client.Close()
	handle.State = HandleDestroyed
	return err
}

func (p *SSHProvider) Alive(handle *Handle) bool {
	return sshAlive(handle.internal.(*ssh.Client))
}

// sshDial connects to host (host:port) as user with the auth derived from opts
// (password and/or key_file). The dial is ctx-cancellable.
func sshDial(ctx context.Context, host, user string, opts SpawnOpts) (*ssh.Client, error) {
	auth, err := sshAuth(opts)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // Phase 3b: explicit trusted_hosts later.
		Timeout:         30 * time.Second,
	}

	type result struct {
		client *ssh.Client
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		client, err := ssh.Dial("tcp", host, cfg)
		ch <- result{client: client, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("ssh dial %s: %w", host, r.err)
		}
		return r.client, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func sshAuth(opts SpawnOpts) ([]ssh.AuthMethod, error) {
	var auth []ssh.AuthMethod
	if opts.Password != "" {
		auth = append(auth, ssh.Password(opts.Password))
	}
	if opts.KeyFile != "" {
		b, err := os.ReadFile(opts.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("read ssh key %s: %w", opts.KeyFile, err)
		}
		signer, err := ssh.ParsePrivateKey(b)
		if err != nil {
			return nil, fmt.Errorf("parse ssh key %s: %w", opts.KeyFile, err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if len(auth) == 0 {
		return nil, fmt.Errorf("ssh: password or key_file is required")
	}
	return auth, nil
}

// sshExec runs cmdStr over an established SSH client and returns its output.
func sshExec(ctx context.Context, client *ssh.Client, cmdStr string) (*ExecResult, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh new session: %w", err)
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	start := time.Now()
	errCh := make(chan error, 1)
	go func() { errCh <- sess.Run(cmdStr) }()

	exitCode := 0
	select {
	case err := <-errCh:
		if err != nil {
			if exitErr, ok := err.(*ssh.ExitError); ok {
				exitCode = exitErr.ExitStatus()
			} else {
				return nil, fmt.Errorf("ssh exec: %w", err)
			}
		}
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return nil, ctx.Err()
	}

	return &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Duration: time.Since(start).Milliseconds(),
	}, nil
}

// sshRead reads a file at path over an established SSH client.
func sshRead(ctx context.Context, client *ssh.Client, path string) ([]byte, error) {
	res, err := sshExec(ctx, client, "cat "+shellQuote(path))
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("ssh read %s failed: %s", path, res.Stderr)
	}
	return []byte(res.Stdout), nil
}

// sshWrite writes content to path over an established SSH client.
func sshWrite(ctx context.Context, client *ssh.Client, path string, content []byte) error {
	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("ssh new session: %w", err)
	}
	defer sess.Close()

	sess.Stdin = bytes.NewReader(content)
	cmd := "cat > " + shellQuote(path)
	errCh := make(chan error, 1)
	go func() { errCh <- sess.Run(cmd) }()
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("ssh write %s: %w", path, err)
		}
		return nil
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return ctx.Err()
	}
}

// sshAlive reports whether the SSH client's keepalive request succeeds.
func sshAlive(client *ssh.Client) bool {
	_, _, err := client.SendRequest("keepalive@agentflow", true, nil)
	return err == nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
