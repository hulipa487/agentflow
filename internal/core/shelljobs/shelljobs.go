// Package shelljobs runs shell commands for sessions as one-off sandbox jobs.
//
// It is the meeting point of three things that know nothing about each other:
// the files store's project trees (internal/core/workspace lays one out on
// disk and writes changes back), the declared environments, and the sandbox
// runner (internal/drivers/sandbox). A job is queued, given the project's
// workspace and an environment's root filesystem, run once with a time limit,
// and its changes are stored. Nothing of it remains afterwards except what it
// wrote into the project.
package shelljobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"agentflow/internal/core/files"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/workspace"
	"agentflow/internal/drivers/sandbox"
)

// ManifestPath is where a project may customise the shell profile its agent
// is bound to.
const ManifestPath = ".agentflow/shell.yaml"

// DefaultProject is the project a job runs in when neither the call nor the
// profile names one.
const DefaultProject = "workspace"

// Config is the deployment's sandbox settings (runtime.sandbox).
type Config struct {
	Concurrency          int           // jobs running at once on this instance
	PerScope             int           // jobs running at once for one user (or agent, without a user)
	QueueTimeout         time.Duration // how long a job may wait for its turn
	Timeout              time.Duration // a job's time limit when nothing sets one
	MaxTimeout           time.Duration // the most a synchronous job may ask for
	BackgroundMaxTimeout time.Duration // the most a background job may ask for
	Limits               sandbox.Limits
	WriteBack            workspace.Limits
}

// withDefaults fills in what a deployment left unset.
func (c Config) withDefaults() Config {
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.PerScope <= 0 {
		c.PerScope = 2
	}
	if c.PerScope > c.Concurrency {
		c.PerScope = c.Concurrency
	}
	if c.QueueTimeout <= 0 {
		c.QueueTimeout = 30 * time.Second
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	if c.MaxTimeout <= 0 {
		c.MaxTimeout = 10 * time.Minute
	}
	if c.Timeout > c.MaxTimeout {
		c.Timeout = c.MaxTimeout
	}
	if c.BackgroundMaxTimeout < c.MaxTimeout {
		c.BackgroundMaxTimeout = 2 * time.Hour
		if c.BackgroundMaxTimeout < c.MaxTimeout {
			c.BackgroundMaxTimeout = c.MaxTimeout
		}
	}
	if c.Limits.PIDs <= 0 {
		c.Limits.PIDs = 512
	}
	if c.Limits.MemoryBytes <= 0 {
		c.Limits.MemoryBytes = 1 << 30
	}
	return c
}

// Profile is an agent's bound shell profile, as far as a job is concerned.
type Profile struct {
	Environment string
	Project     string
	WorkDir     string
	Env         map[string]string
	Timeout     time.Duration
	Ignore      []string
	MemoryBytes int64
	CPUs        float64
	Network     string
}

// Request is one command to run.
type Request struct {
	// Scope is the files-store scope the turn acts in. The engine resolves
	// it; a loop cannot name one.
	Scope string
	// Owner is the session the job belongs to.
	Owner   string
	Command string
	Profile Profile

	// Per-call settings. Each overrides the project manifest, which overrides
	// the profile.
	Project     string
	Environment string
	WorkDir     string
	Env         map[string]string
	Timeout     time.Duration
	// Mount is where the workspace appears in the sandbox; empty is /workspace.
	Mount string
	// Background lifts the time limit to the background ceiling.
	Background bool
}

// Result is a finished job.
type Result struct {
	ExitCode    int
	Stdout      string
	Stderr      string
	Duration    time.Duration
	Queued      time.Duration
	TimedOut    bool
	Truncated   bool
	Project     string
	Environment string
	Changes     *workspace.Changes
}

// Service runs jobs.
type Service struct {
	cfg    Config
	runner sandbox.Runner
	envs   *sandbox.Envs
	ws     *workspace.Manager
	fm     *files.Manager
	log    *slog.Logger

	slots    chan struct{} // instance-wide concurrency
	projects *keyed        // one job per project workspace
	scopes   *keyed        // a bounded number per user

	bg *background
}

// New builds the service. fm may be nil when the files store is disabled, in
// which case every job fails saying so.
func New(cfg Config, runner sandbox.Runner, envs *sandbox.Envs, ws *workspace.Manager, fm *files.Manager, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg = cfg.withDefaults()
	s := &Service{
		cfg:      cfg,
		runner:   runner,
		envs:     envs,
		ws:       ws,
		fm:       fm,
		log:      log.With("module", "shelljobs"),
		slots:    make(chan struct{}, cfg.Concurrency),
		projects: newKeyed(),
		scopes:   newKeyed(),
	}
	s.bg = newBackground(s)
	return s
}

// Available reports why no job can run on this instance, or nil.
func (s *Service) Available() error {
	if s.fm == nil {
		return fmt.Errorf("%w: the files store is disabled, and a job's workspace is a project in it", sandbox.ErrUnavailable)
	}
	return s.runner.Available()
}

// manifest is a project's .agentflow/shell.yaml. It can choose among what the
// operator declared and set what is safe for a user to set; it has no field
// for an image, a network policy or a resource limit, and an unknown field is
// an error rather than a silent no-op.
type manifest struct {
	Environment string            `yaml:"environment"`
	Env         map[string]string `yaml:"env"`
	WorkDir     string            `yaml:"workdir"`
	Timeout     string            `yaml:"timeout"`
	Ignore      []string          `yaml:"ignore"`
}

func (s *Service) manifest(ctx context.Context, scope, project string) (manifest, error) {
	var m manifest
	e, err := s.fm.Get(ctx, scope, project, ManifestPath)
	if errors.Is(err, files.ErrNotFound) {
		return m, nil
	}
	if err != nil {
		return m, fmt.Errorf("shell: read %s: %w", ManifestPath, err)
	}
	raw, err := s.fm.ReadBlob(ctx, e.Handle, 64<<10)
	if err != nil {
		return m, fmt.Errorf("shell: read %s: %w", ManifestPath, err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil && err.Error() != "EOF" {
		return m, fmt.Errorf("shell: %s in project %q is not valid: %v (it may set environment, env, workdir, timeout and ignore)", ManifestPath, project, err)
	}
	return m, nil
}

// plan is a request with every setting resolved.
type plan struct {
	scope, project string
	environment    string
	workdir        string
	env            map[string]string // profile, then manifest, then call
	timeout        time.Duration
	ignore         workspace.Ignore
	limits         sandbox.Limits
	network        string
}

func (s *Service) plan(ctx context.Context, req Request) (plan, error) {
	p := plan{scope: req.Scope, network: req.Profile.Network}
	if strings.TrimSpace(req.Command) == "" {
		return p, errors.New("shell: missing command")
	}
	if req.Scope == "" {
		return p, errors.New("shell: the turn has no scope to run a job in")
	}
	p.project = firstNonEmpty(req.Project, req.Profile.Project, DefaultProject)
	if err := validProject(p.project); err != nil {
		return p, err
	}
	man, err := s.manifest(ctx, req.Scope, p.project)
	if err != nil {
		return p, err
	}

	p.environment = firstNonEmpty(req.Environment, man.Environment, req.Profile.Environment)
	if p.environment == "" {
		return p, fmt.Errorf("%w: no environment is configured for this agent's shell profile (declared: %s)",
			sandbox.ErrUnavailable, strings.Join(s.envs.Names(), ", "))
	}
	if !s.envs.Has(p.environment) {
		return p, fmt.Errorf("shell: unknown environment %q (declared: %s)", p.environment, strings.Join(s.envs.Names(), ", "))
	}
	p.workdir = firstNonEmpty(req.WorkDir, man.WorkDir, req.Profile.WorkDir)

	p.env = map[string]string{}
	for _, layer := range []map[string]string{req.Profile.Env, man.Env, req.Env} {
		for k, v := range layer {
			if k == "" || strings.ContainsAny(k, "=\x00") {
				return p, fmt.Errorf("shell: %q is not a usable environment variable name", k)
			}
			p.env[k] = v
		}
	}

	ceiling := s.cfg.MaxTimeout
	if req.Background {
		ceiling = s.cfg.BackgroundMaxTimeout
	}
	p.timeout = req.Timeout
	if p.timeout <= 0 && man.Timeout != "" {
		d, err := time.ParseDuration(man.Timeout)
		if err != nil || d <= 0 {
			return p, fmt.Errorf("shell: %s in project %q has an unusable timeout %q (use a duration such as 90s or 5m)", ManifestPath, p.project, man.Timeout)
		}
		p.timeout = d
	}
	if p.timeout <= 0 {
		p.timeout = req.Profile.Timeout
	}
	if p.timeout <= 0 {
		p.timeout = s.cfg.Timeout
		if req.Background {
			p.timeout = ceiling
		}
	}
	if p.timeout > ceiling {
		p.timeout = ceiling
	}

	p.ignore = workspace.NewIgnore(append(append([]string{}, req.Profile.Ignore...), man.Ignore...))

	// A profile may lower a limit, never raise it past the deployment's.
	p.limits = s.cfg.Limits
	if m := req.Profile.MemoryBytes; m > 0 && m < p.limits.MemoryBytes {
		p.limits.MemoryBytes = m
	}
	if c := req.Profile.CPUs; c > 0 && (p.limits.CPUs == 0 || c < p.limits.CPUs) {
		p.limits.CPUs = c
	}
	return p, nil
}

// Run executes one job and returns its result. An error means there is no
// result: the job could not be started, waited too long for its turn, or its
// sandbox failed. A command that exits non-zero or runs into its time limit
// is a result.
func (s *Service) Run(ctx context.Context, req Request) (*Result, error) {
	return s.run(ctx, req, nil)
}

func (s *Service) run(ctx context.Context, req Request, onOutput func(stream string, p []byte)) (*Result, error) {
	if err := s.Available(); err != nil {
		metrics.Inc("agentflow_shell_jobs_unavailable")
		return nil, err
	}
	p, err := s.plan(ctx, req)
	if err != nil {
		return nil, err
	}
	// Resolved before queueing: the first use of an image downloads it, and
	// that wait belongs to the caller, not to a slot other jobs could use.
	env, err := s.envs.Resolve(ctx, p.environment)
	if err != nil {
		return nil, err
	}

	queuedAt := time.Now()
	release, err := s.queue(ctx, p)
	if err != nil {
		return nil, err
	}
	defer release()
	queued := time.Since(queuedAt)

	ws, err := s.ws.Prepare(ctx, p.scope, p.project, p.ignore)
	if err != nil {
		return nil, err
	}
	if !ws.Has(p.workdir) {
		return nil, fmt.Errorf("shell: workdir %q does not exist in project %q", p.workdir, p.project)
	}

	vars := map[string]string{}
	for _, layer := range []map[string]string{env.Env, p.env} {
		for k, v := range layer {
			vars[k] = v
		}
	}
	spec := sandbox.Spec{
		ID:        newID(),
		Command:   req.Command,
		Env:       vars,
		Rootfs:    env.Rootfs,
		Workspace: ws.Dir,
		Mount:     req.Mount,
		WorkDir:   p.workdir,
		Timeout:   p.timeout,
		Limits:    p.limits,
		Network:   p.network,
		OnOutput:  onOutput,
	}
	metrics.Inc("agentflow_shell_jobs_started")
	res, runErr := s.runner.Run(ctx, spec)

	// Whatever the command did before it ended — normally, at its time limit,
	// or with its caller gone — is in the directory now, and the directory is
	// only a cache. It is written back in every case, so the project never
	// depends on which instance a job happened to die on. The caller's context
	// may be the thing that ended the job, so write-back does not use it.
	collectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	changes, collectErr := ws.Collect(collectCtx, s.cfg.WriteBack)
	cancel()

	if runErr != nil {
		metrics.Inc("agentflow_shell_jobs_failed")
		if collectErr == nil && len(changes.Changed)+len(changes.Deleted) > 0 {
			runErr = fmt.Errorf("%w (%d file(s) it had already changed were kept)", runErr, len(changes.Changed)+len(changes.Deleted))
		}
		s.log.Warn("shell job failed", "job", spec.ID, "owner", req.Owner, "project", p.project, "err", runErr)
		return nil, runErr
	}
	if collectErr != nil {
		metrics.Inc("agentflow_shell_jobs_failed")
		return nil, fmt.Errorf("shell: the command ran (exit %d) but its changes could not be stored: %w", res.ExitCode, collectErr)
	}
	if res.TimedOut {
		metrics.Inc("agentflow_shell_jobs_timed_out")
	}
	s.log.Info("shell job finished", "job", spec.ID, "owner", req.Owner, "project", p.project,
		"environment", p.environment, "exit", res.ExitCode, "timed_out", res.TimedOut,
		"queued_ms", queued.Milliseconds(), "ran_ms", res.Duration.Milliseconds(),
		"changed", len(changes.Changed), "deleted", len(changes.Deleted),
		"conflicts", len(changes.Conflicts), "skipped", len(changes.Skipped))
	return &Result{
		ExitCode:    res.ExitCode,
		Stdout:      res.Stdout,
		Stderr:      res.Stderr,
		Duration:    res.Duration,
		Queued:      queued,
		TimedOut:    res.TimedOut,
		Truncated:   res.Truncated,
		Project:     p.project,
		Environment: p.environment,
		Changes:     changes,
	}, nil
}

// queue waits for the job's turn: its project first, then its user's share,
// then a slot on this instance. In that order a job waiting behind another on
// the same project holds nothing anyone else needs.
func (s *Service) queue(ctx context.Context, p plan) (release func(), err error) {
	waitCtx, cancel := context.WithTimeout(ctx, s.cfg.QueueTimeout)
	defer cancel()
	key := workspace.Key(p.scope, p.project)

	fail := func(what string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		metrics.Inc("agentflow_shell_jobs_queue_timeouts")
		return fmt.Errorf("shell: still waiting for %s after %s; try again, or raise runtime.sandbox.queue_timeout", what, s.cfg.QueueTimeout)
	}
	releaseProject, err := s.projects.acquire(waitCtx, key, 1)
	if err != nil {
		return nil, fail(fmt.Sprintf("another command on project %q to finish", p.project))
	}
	releaseScope, err := s.scopes.acquire(waitCtx, p.scope, s.cfg.PerScope)
	if err != nil {
		releaseProject()
		return nil, fail("this user's other commands to finish")
	}
	select {
	case s.slots <- struct{}{}:
	case <-waitCtx.Done():
		releaseScope()
		releaseProject()
		return nil, fail(fmt.Sprintf("one of the %d sandbox slots", s.cfg.Concurrency))
	}
	return func() {
		<-s.slots
		releaseScope()
		releaseProject()
	}, nil
}

func validProject(name string) error {
	if name == "" || len(name) > 128 || name == "." || name == ".." || strings.ContainsAny(name, "|/\\\x00") {
		return fmt.Errorf("shell: %q is not a usable project name", name)
	}
	return nil
}

// bytesToValidUTF8 drops a partial character from either end of a cut and
// replaces anything else that is not text, so the result can go into JSON.
func bytesToValidUTF8(b []byte) []byte {
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	return bytes.ToValidUTF8(b, []byte("\uFFFD"))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// newID mints a job id: short, unique, and safe as a file name and as a
// sandbox name.
func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "j" + hex.EncodeToString(b[:])
}

// Environments lists the declared environment names, sorted.
func (s *Service) Environments() []string { return s.envs.Names() }

// HasEnvironment reports whether an environment is declared.
func (s *Service) HasEnvironment(name string) bool { return s.envs.Has(name) }
