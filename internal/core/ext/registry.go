package ext

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"

	"agentflow/internal/core/tools"
)

type apiRunner interface {
	Invoke(ctx context.Context, action string, args map[string]any) (any, error)
	Close(ctx context.Context) error
}

type registeredAPI struct {
	manifest Manifest
	runner   apiRunner
}

// APIView describes one registered External API package without secrets.
type APIView struct {
	Name        string                `json:"name"`
	Version     string                `json:"version,omitempty"`
	Description string                `json:"description,omitempty"`
	Runtime     RuntimeKind           `json:"runtime"`
	Actions     map[string]ActionSpec `json:"actions"`
}

// BatchCall identifies one External API invocation inside CallBatch.
type BatchCall struct {
	API    string         `json:"api"`
	Action string         `json:"action"`
	Args   map[string]any `json:"args,omitempty"`
}

// BatchResult holds the outcome of one entry in CallBatch (ordered 1-to-1 with requests).
type BatchResult struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// Registry compiles, stores, and invokes registered Script/Wasm External APIs
// under a shared concurrency bound and host-enforced capabilities.
type Registry struct {
	mu   sync.RWMutex
	apis map[string]*registeredAPI
	deps HostDeps
	sem  chan struct{}
}

// NewRegistry builds an External API registry backed by deps.
func NewRegistry(deps HostDeps) *Registry {
	limit := runtime.GOMAXPROCS(0) * 2
	if limit < 8 {
		limit = 8
	}
	return &Registry{
		apis: map[string]*registeredAPI{},
		deps: deps,
		sem:  make(chan struct{}, limit),
	}
}

// Register validates, compiles, and installs an External API package described by m.
func (r *Registry) Register(m Manifest, baseDir string) error {
	if err := ValidateManifest(m); err != nil {
		return err
	}
	var runner apiRunner
	var err error
	switch m.Runtime {
	case RuntimeScript:
		runner, err = newScriptRunner(m, baseDir, r.deps)
	case RuntimeWasm:
		runner, err = newWasmRunner(m, baseDir, r.deps)
	default:
		return fmt.Errorf("api %q: unsupported runtime %q", m.Name, m.Runtime)
	}
	if err != nil {
		return err
	}

	r.mu.Lock()
	old := r.apis[m.Name]
	r.apis[m.Name] = &registeredAPI{manifest: m, runner: runner}
	r.mu.Unlock()

	if old != nil && old.runner != nil {
		_ = old.runner.Close(context.Background())
	}
	return nil
}

// RegisterDir loads <dir>/manifest.yaml and registers the package.
func (r *Registry) RegisterDir(dir string) error {
	m, err := LoadManifestDir(dir)
	if err != nil {
		return err
	}
	return r.Register(m, dir)
}

// List returns sorted metadata views of all registered External APIs.
func (r *Registry) List() []APIView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.apis))
	for name := range r.apis {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]APIView, 0, len(names))
	for _, name := range names {
		m := r.apis[name].manifest
		actions := make(map[string]ActionSpec, len(m.Actions))
		for k, v := range m.Actions {
			actions[k] = v
		}
		out = append(out, APIView{
			Name:        m.Name,
			Version:     m.Version,
			Description: m.Description,
			Runtime:     m.Runtime,
			Actions:     actions,
		})
	}
	return out
}

// Call invokes one External API action synchronously under the registry's CPU semaphore.
func (r *Registry) Call(ctx context.Context, apiName, action string, args map[string]any) (any, error) {
	if r == nil {
		return nil, fmt.Errorf("external api registry is not configured")
	}
	r.mu.RLock()
	entry, ok := r.apis[apiName]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown external api %q", apiName)
	}

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return entry.runner.Invoke(ctx, action, args)
}

// CallBatch fans out multiple External API calls concurrently and returns results
// in the exact order of calls.
func (r *Registry) CallBatch(ctx context.Context, calls []BatchCall) []BatchResult {
	out := make([]BatchResult, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(idx int, c BatchCall) {
			defer wg.Done()
			data, err := r.Call(ctx, c.API, c.Action, c.Args)
			if err != nil {
				out[idx] = BatchResult{OK: false, Error: err.Error()}
				return
			}
			out[idx] = BatchResult{OK: true, Data: data}
		}(i, call)
	}
	wg.Wait()
	return out
}

// RegisterTools exposes actions marked expose_as_tool: true on toolReg under
// "api:<pkg>.<action>".
func (r *Registry) RegisterTools(toolReg *tools.Registry) {
	if r == nil || toolReg == nil {
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.apis))
	for name := range r.apis {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, pkgName := range names {
		m := r.apis[pkgName].manifest
		actNames := make([]string, 0, len(m.Actions))
		for actName := range m.Actions {
			actNames = append(actNames, actName)
		}
		sort.Strings(actNames)

		for _, actName := range actNames {
			spec := m.Actions[actName]
			if !spec.ExposeAsTool {
				continue
			}
			pkg := pkgName
			act := actName
			reqCap := spec.RequiresCapability
			if reqCap == "" {
				reqCap = "apis"
			}
			toolReg.Register(tools.ToolSpec{
				Name:               fmt.Sprintf("api:%s.%s", pkg, act),
				Description:        spec.Description,
				Parameters:         spec.Parameters,
				NeedsConfirm:       spec.NeedsConfirm,
				RequiresCapability: reqCap,
				Invoke: func(ctx context.Context, args map[string]any) (any, error) {
					return r.Call(ctx, pkg, act, args)
				},
			})
		}
	}
}

// Close shuts down all underlying VM/Wasm runtimes cleanly.
func (r *Registry) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	runners := make([]apiRunner, 0, len(r.apis))
	for _, entry := range r.apis {
		if entry.runner != nil {
			runners = append(runners, entry.runner)
		}
	}
	r.apis = map[string]*registeredAPI{}
	r.mu.Unlock()

	var firstErr error
	for _, runner := range runners {
		if err := runner.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
