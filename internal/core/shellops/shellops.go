// Package shellops is what a shell call means, shared by the two ways of
// making one: the Lua ops (internal/core/caps) and the model-facing tools
// (internal/core/tools).
//
// There are two kinds of shell. A sandbox shell is not a thing that exists:
// each command is a one-off job on a project in the files store, and a
// "handle" to one is only the options a loop chose, encoded into the id so
// that it needs no storage and works on any instance. An SSH shell is a real
// connection to a real machine and keeps the handle semantics it always had.
// Everything here routes on that difference so the callers do not have to.
package shellops

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"agentflow/internal/core/files"
	"agentflow/internal/core/session"
	"agentflow/internal/core/shelljobs"
	"agentflow/internal/drivers/sandbox"
	"agentflow/internal/drivers/shell"
)

// Providers.
const (
	Sandbox = "sandbox"
	SSH     = "ssh"
)

// handlePrefix marks a sandbox handle. No SSH handle starts with it.
const handlePrefix = "sandbox:"

// maxHandle bounds an encoded handle: it travels through Lua and into logs.
const maxHandle = 8 << 10

// maxRead bounds what a file read hands back to a model or a loop as text.
const maxRead = 256 << 10

// Front is the shell as the op and tool layers see it.
type Front struct {
	SSH   *shell.Manager     // real handles; may be nil
	Jobs  *shelljobs.Service // sandbox jobs; may be nil
	Files *files.Manager     // may be nil when the files store is disabled
}

// Options are a sandbox shell's settings, as a loop gave them to shell.spawn
// or to one shell.run call. Zero fields fall back to the project's manifest
// and then to the agent's profile.
type Options struct {
	Project     string            `json:"p,omitempty"`
	Environment string            `json:"e,omitempty"`
	WorkDir     string            `json:"w,omitempty"`
	Mount       string            `json:"m,omitempty"`
	Env         map[string]string `json:"v,omitempty"`
}

// Encode turns options into a handle id.
func (o Options) Encode() (string, error) {
	raw, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	id := handlePrefix + base64.RawURLEncoding.EncodeToString(raw)
	if len(id) > maxHandle {
		return "", fmt.Errorf("shell.spawn: the options are too large to carry in a handle (%d bytes); pass large env tables to shell.run instead", len(id))
	}
	return id, nil
}

// IsSandboxHandle reports whether an id names a sandbox shell.
func IsSandboxHandle(id string) bool { return strings.HasPrefix(id, handlePrefix) }

// DecodeHandle reads a sandbox handle back into its options.
func DecodeHandle(id string) (Options, error) {
	var o Options
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, handlePrefix))
	if err != nil || !IsSandboxHandle(id) || json.Unmarshal(raw, &o) != nil {
		return o, fmt.Errorf("shell handle %q is not one shell.spawn returned", clip(id, 40))
	}
	return o, nil
}

// Profile reads an agent's bound sandbox shell profile from the op context.
// ok is false when the agent has no sandbox shell profile bound.
func Profile(ctx context.Context) (provider string, p shelljobs.Profile, raw map[string]any, ok bool) {
	raw = session.ShellFromCtx(ctx)
	if raw == nil {
		return "", p, nil, false
	}
	str := func(key string) string {
		s, _ := raw[key].(string)
		return s
	}
	provider = str("provider")
	if provider == "" {
		provider = Sandbox
	}
	p = shelljobs.Profile{
		Environment: str("environment"),
		Project:     str("project"),
		WorkDir:     str("workdir"),
		Network:     str("network"),
	}
	p.Env, _ = raw["env"].(map[string]string)
	p.Ignore, _ = raw["ignore"].([]string)
	if d, ok := raw["timeout"].(time.Duration); ok {
		p.Timeout = d
	}
	if n, ok := raw["memory_bytes"].(int64); ok {
		p.MemoryBytes = n
	}
	if c, ok := raw["cpu_limit"].(float64); ok {
		p.CPUs = c
	}
	return provider, p, raw, true
}

// SSHProfile reads an agent's bound SSH profile from the op context into
// shell.SpawnOpts. ok is false when the agent has no SSH profile bound.
func SSHProfile(ctx context.Context) (shell.SpawnOpts, bool) {
	raw := session.SSHFromCtx(ctx)
	if raw == nil {
		return shell.SpawnOpts{}, false
	}
	str := func(key string) string {
		s, _ := raw[key].(string)
		return s
	}
	opts := shell.SpawnOpts{
		WorkDir:  str("workdir"),
		Host:     str("host"),
		User:     str("user"),
		Password: str("password"),
		KeyFile:  str("key_file"),
	}
	opts.Env, _ = raw["env"].(map[string]string)
	return opts, true
}

// Scope is the files-store scope a turn acts in: the user's when the turn has
// one, otherwise the agent's own. A loop can never name it.
func Scope(ctx context.Context, agent string) string {
	if p := session.ScopePrefixFromCtx(ctx); p != "" {
		return p
	}
	return "agent:" + agent
}

// AgentOf extracts the agent name from a session key ("agent|key").
func AgentOf(owner string) string {
	agent, _, _ := strings.Cut(owner, "|")
	return agent
}

// errNoSandbox is what a sandbox call gets on a build or host without one.
func (f *Front) jobs() (*shelljobs.Service, error) {
	if f.Jobs == nil {
		return nil, fmt.Errorf("%w: this engine was started without the sandbox", sandbox.ErrUnavailable)
	}
	return f.Jobs, nil
}

// request builds a job request from the context, the profile and the call's
// own options.
func (f *Front) request(ctx context.Context, agent, command string, o Options, timeout time.Duration) (shelljobs.Request, error) {
	owner := session.OwnerFromCtx(ctx)
	if owner == "" {
		return shelljobs.Request{}, errors.New("shell: no owner in context")
	}
	_, profile, _, _ := Profile(ctx)
	return shelljobs.Request{
		Scope:       Scope(ctx, agent),
		Owner:       owner,
		Command:     command,
		Profile:     profile,
		Project:     o.Project,
		Environment: o.Environment,
		WorkDir:     o.WorkDir,
		Env:         o.Env,
		Mount:       o.Mount,
		Timeout:     timeout,
	}, nil
}

// Run executes one sandbox job and returns its result as the table a loop or
// a model receives.
func (f *Front) Run(ctx context.Context, agent, command string, o Options, timeout time.Duration) (map[string]any, error) {
	jobs, err := f.jobs()
	if err != nil {
		return nil, err
	}
	req, err := f.request(ctx, agent, command, o, timeout)
	if err != nil {
		return nil, err
	}
	res, err := jobs.Run(ctx, req)
	if err != nil {
		return nil, err
	}
	return ResultMap(res), nil
}

// Start queues a background job. done receives its final state.
func (f *Front) Start(ctx context.Context, agent, command string, o Options, timeout time.Duration, done func(shelljobs.Job)) (shelljobs.Job, error) {
	jobs, err := f.jobs()
	if err != nil {
		return shelljobs.Job{}, err
	}
	req, err := f.request(ctx, agent, command, o, timeout)
	if err != nil {
		return shelljobs.Job{}, err
	}
	return jobs.Start(ctx, req, done)
}

// ResultMap is a job result in the shape shell.exec has always returned, with
// what a job adds: whether it hit its limits, and what it did to the project.
func ResultMap(res *shelljobs.Result) map[string]any {
	out := map[string]any{
		"ok":          true,
		"stdout":      res.Stdout,
		"stderr":      res.Stderr,
		"exit_code":   res.ExitCode,
		"duration_ms": res.Duration.Milliseconds(),
		"timed_out":   res.TimedOut,
		"truncated":   res.Truncated,
		"project":     res.Project,
		"environment": res.Environment,
	}
	if ch := res.Changes; ch != nil {
		out["changed"], out["deleted"], out["conflicts"], out["skipped"] = ch.Changed, ch.Deleted, ch.Conflicts, ch.Skipped
	}
	return out
}

// JobMap is a background job as a loop sees it.
func JobMap(j shelljobs.Job) map[string]any {
	out := map[string]any{
		"ok":          true,
		"id":          j.ID,
		"state":       j.State,
		"project":     j.Project,
		"command":     j.Command,
		"started_at":  j.StartedAt,
		"stdout":      j.Stdout,
		"stderr":      j.Stderr,
		"exit_code":   j.ExitCode,
		"timed_out":   j.TimedOut,
		"truncated":   j.Truncated,
		"duration_ms": j.DurationMS,
	}
	if j.EndedAt != 0 {
		out["ended_at"] = j.EndedAt
	}
	if j.Error != "" {
		out["error"] = j.Error
	}
	if ch := j.Changes; ch != nil {
		out["changed"], out["deleted"], out["conflicts"], out["skipped"] = ch.Changed, ch.Deleted, ch.Conflicts, ch.Skipped
	}
	return out
}

// project resolves which project a sandbox shell's files are in.
func project(ctx context.Context, o Options) string {
	if o.Project != "" {
		return o.Project
	}
	if _, p, _, ok := Profile(ctx); ok && p.Project != "" {
		return p.Project
	}
	return shelljobs.DefaultProject
}

// WorkspacePath maps a path a loop or model wrote to a path in the project.
// Commands see the project at the mount point, so "/workspace/src/a.py" and
// "src/a.py" are the same file; an absolute path anywhere else is not in the
// project and cannot be read or written without running a command.
func WorkspacePath(o Options, p string) (string, error) {
	mount := o.Mount
	if mount == "" {
		mount = sandbox.DefaultMount
	}
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("missing path")
	}
	if path.IsAbs(p) {
		clean := path.Clean(p)
		if clean != mount && !strings.HasPrefix(clean, mount+"/") {
			return "", fmt.Errorf("%q is outside the workspace: a sandbox has no files of its own, only the project mounted at %s", p, mount)
		}
		p = strings.TrimPrefix(strings.TrimPrefix(clean, mount), "/")
	} else if o.WorkDir != "" {
		// A relative path means what it would mean to a command.
		p = path.Join(o.WorkDir, p)
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%q is not a file inside the workspace", p)
	}
	return clean, nil
}

// Write stores a file in a sandbox shell's project.
func (f *Front) Write(ctx context.Context, agent string, o Options, p string, content []byte) (string, error) {
	if f.Files == nil {
		return "", errors.New("files store unavailable (disabled at boot)")
	}
	rel, err := WorkspacePath(o, p)
	if err != nil {
		return "", err
	}
	if _, err := f.Files.Put(ctx, Scope(ctx, agent), project(ctx, o), rel, bytes.NewReader(content), "text/plain"); err != nil {
		return "", err
	}
	return rel, nil
}

// Read returns a file of a sandbox shell's project as text.
func (f *Front) Read(ctx context.Context, agent string, o Options, p string) (content, rel string, truncated bool, err error) {
	if f.Files == nil {
		return "", "", false, errors.New("files store unavailable (disabled at boot)")
	}
	if rel, err = WorkspacePath(o, p); err != nil {
		return "", "", false, err
	}
	e, err := f.Files.Get(ctx, Scope(ctx, agent), project(ctx, o), rel)
	if err != nil {
		return "", rel, false, fmt.Errorf("%s: %w", rel, err)
	}
	b, err := f.Files.ReadBlob(ctx, e.Handle, f.Files.MaxFileBytes())
	if err != nil {
		return "", rel, false, fmt.Errorf("%s: %w", rel, err)
	}
	if len(b) > maxRead {
		b, truncated = b[:maxRead], true
		for len(b) > 0 && !utf8.Valid(b) {
			b = b[:len(b)-1]
		}
	}
	if !utf8.Valid(b) {
		return "", rel, false, fmt.Errorf("%s is not a text file", rel)
	}
	return string(b), rel, truncated, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// WorkspaceDir maps a working directory a loop gave — relative, or absolute
// under the mount point, as it would have been in a container — to a
// directory relative to the workspace root.
func WorkspaceDir(mount, dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}
	if mount == "" {
		mount = sandbox.DefaultMount
	}
	clean := path.Clean(dir)
	if path.IsAbs(clean) {
		if clean != mount && !strings.HasPrefix(clean, mount+"/") {
			return "", fmt.Errorf("workdir %q is outside the workspace, which is mounted at %s", dir, mount)
		}
		clean = strings.TrimPrefix(strings.TrimPrefix(clean, mount), "/")
	}
	if clean == "" || clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("workdir %q is outside the workspace", dir)
	}
	return clean, nil
}
