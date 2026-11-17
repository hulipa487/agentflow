package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// EnvSpec declares one environment: where its root filesystem comes from and
// which variables every job in it starts with. Exactly one of Image and
// Rootfs is set.
type EnvSpec struct {
	// Image is an OCI image pinned by digest. It is downloaded once and kept.
	Image string
	// Rootfs is a directory on this host that already holds a root filesystem.
	Rootfs string
	// Env is added to (and overrides) what the image declares.
	Env map[string]string
}

// Environment is a resolved environment, ready to run jobs in.
type Environment struct {
	Name   string
	Rootfs string
	Env    map[string]string
}

// Envs resolves environment names to root filesystems. An image is pulled the
// first time an environment is used, into a directory named by its digest:
// the digest is the cache key, so changing the pin in the config is all it
// takes to move to a new image, and two environments on one image share it.
type Envs struct {
	dir    string
	specs  map[string]EnvSpec
	client *http.Client
	scheme string
	log    *slog.Logger

	mu    sync.Mutex
	pulls map[string]*sync.Mutex // per image digest: one download at a time
}

// NewEnvs builds the environment store over the sandbox data directory.
func NewEnvs(dir string, specs map[string]EnvSpec, log *slog.Logger) *Envs {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Envs{
		dir:   dir,
		specs: specs,
		// No overall timeout: a layer can be large. The pull's context bounds it.
		client: &http.Client{Transport: http.DefaultTransport},
		scheme: "https",
		log:    log.With("module", "sandbox.envs"),
		pulls:  map[string]*sync.Mutex{},
	}
}

// Names lists the declared environments, sorted.
func (e *Envs) Names() []string {
	out := make([]string, 0, len(e.specs))
	for name := range e.specs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Has reports whether an environment is declared.
func (e *Envs) Has(name string) bool {
	_, ok := e.specs[name]
	return ok
}

// Resolve returns an environment ready for a job, pulling its image first
// when this is the first use on this host.
func (e *Envs) Resolve(ctx context.Context, name string) (*Environment, error) {
	spec, ok := e.specs[name]
	if !ok {
		return nil, fmt.Errorf("%w: unknown environment %q (declared: %s)", ErrUnavailable, name, strings.Join(e.Names(), ", "))
	}
	env := map[string]string{}
	rootfs := spec.Rootfs
	if spec.Image != "" {
		ref, err := ParseImageRef(spec.Image)
		if err != nil {
			return nil, fmt.Errorf("%w: environment %q: %v", ErrUnavailable, name, err)
		}
		var imageEnv []string
		if rootfs, imageEnv, err = e.image(ctx, ref); err != nil {
			return nil, fmt.Errorf("%w: environment %q: %v", ErrUnavailable, name, err)
		}
		for _, kv := range imageEnv {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env[k] = v
			}
		}
	}
	if st, err := os.Stat(rootfs); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%w: environment %q: root filesystem %q is not a directory", ErrUnavailable, name, rootfs)
	}
	for k, v := range spec.Env {
		env[k] = v
	}
	return &Environment{Name: name, Rootfs: rootfs, Env: env}, nil
}

// image returns the unpacked root filesystem of an image, pulling it once.
func (e *Envs) image(ctx context.Context, ref ImageRef) (string, []string, error) {
	base, err := filepath.Abs(filepath.Join(e.dir, "images", strings.TrimPrefix(ref.Digest, "sha256:")))
	if err != nil {
		return "", nil, err
	}
	rootfs, envFile := filepath.Join(base, "rootfs"), filepath.Join(base, "env.json")
	read := func() ([]string, bool) {
		raw, err := os.ReadFile(envFile)
		if err != nil {
			return nil, false
		}
		var env []string
		return env, json.Unmarshal(raw, &env) == nil
	}
	// env.json is written last, after a complete unpack, so its presence is
	// what says the directory beside it can be trusted.
	if env, ok := read(); ok {
		return rootfs, env, nil
	}

	e.mu.Lock()
	lock := e.pulls[ref.Digest]
	if lock == nil {
		lock = &sync.Mutex{}
		e.pulls[ref.Digest] = lock
	}
	e.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	if env, ok := read(); ok {
		return rootfs, env, nil
	}

	// A directory without env.json is an interrupted pull: start over.
	if err := os.RemoveAll(base); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		return "", nil, err
	}
	start := time.Now()
	e.log.Info("pulling environment image", "registry", ref.Registry, "repo", ref.Repo, "digest", ref.Digest)
	env, err := pullImage(ctx, e.client, e.scheme, ref, rootfs)
	if err != nil {
		_ = os.RemoveAll(base)
		return "", nil, fmt.Errorf("pull %s/%s: %w", ref.Registry, ref.Repo, err)
	}
	raw, _ := json.Marshal(env)
	if err := os.WriteFile(envFile, raw, 0o644); err != nil {
		_ = os.RemoveAll(base)
		return "", nil, err
	}
	e.log.Info("environment image ready", "repo", ref.Repo, "took", time.Since(start).Round(time.Millisecond).String())
	return rootfs, env, nil
}
