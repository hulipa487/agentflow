package ext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	"agentflow/internal/config"
	"agentflow/internal/core/execguard"
	"agentflow/internal/core/session"
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
	API            string         `json:"api"`
	Action         string         `json:"action"`
	Args           map[string]any `json:"args,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
}

// CallPolicy configures execution-boundary authorization for External API calls.
type CallPolicy struct {
	Guard          *execguard.Store
	HostedMode     bool
	GrantedCaps    map[string]bool
	ToolsPolicy    config.ToolsPolicy
	Agent          string
	IdempotencyKey string
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
	if _, ok := entry.manifest.Actions[action]; !ok {
		return nil, fmt.Errorf("unknown action %q on api %q", action, apiName)
	}

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return entry.runner.Invoke(ctx, action, args)
}

func extDestination(m Manifest) string {
	base := "api:" + m.Name
	if m.Capabilities.HTTP != nil && len(m.Capabilities.HTTP.AllowedHosts) > 0 {
		return fmt.Sprintf("%s (%s)", base, strings.Join(m.Capabilities.HTTP.AllowedHosts, ","))
	}
	return base
}

func isToolPolicyForbidden(canonical string, policy config.ToolsPolicy) bool {
	for _, f := range policy.Forbidden {
		if f == canonical {
			return true
		}
	}
	if override, ok := policy.Overrides[canonical]; ok {
		if override.Permission != nil && *override.Permission == "forbidden" {
			return true
		}
	}
	return false
}

// AuthorizeAndCall evaluates capability, policy, hosted-mode, approval, and
// idempotency constraints for one External API action invocation before
// running it.
func (r *Registry) AuthorizeAndCall(ctx context.Context, apiName, action string, args map[string]any, pol CallPolicy) (any, map[string]any, error) {
	if r == nil {
		return nil, nil, fmt.Errorf("external api registry is not configured")
	}
	r.mu.RLock()
	entry, ok := r.apis[apiName]
	r.mu.RUnlock()
	if !ok {
		return nil, nil, fmt.Errorf("unknown external api %q", apiName)
	}
	spec, ok := entry.manifest.Actions[action]
	if !ok {
		return nil, nil, fmt.Errorf("unknown action %q on api %q", action, apiName)
	}

	canonical := fmt.Sprintf("api:%s.%s", apiName, action)
	if execguard.IsPreAuthorized(ctx, canonical) {
		data, err := r.Call(ctx, apiName, action, args)
		return data, nil, err
	}

	forbidden := isToolPolicyForbidden(canonical, pol.ToolsPolicy)
	needsConfirm := spec.NeedsConfirm
	kind, _ := execguard.ParseActionKind(spec.Kind)
	if override, ok := pol.ToolsPolicy.Overrides[canonical]; ok {
		if override.NeedsConfirm != nil {
			needsConfirm = *override.NeedsConfirm
		}
		if override.Kind != nil {
			if k, ok := execguard.ParseActionKind(*override.Kind); ok {
				kind = k
			}
		}
	}

	reqCap := spec.RequiresCapability
	if reqCap == "" {
		reqCap = "apis"
	}
	if pol.GrantedCaps == nil && !pol.HostedMode {
		reqCap = ""
	}

	dest := extDestination(entry.manifest)
	_, argsHash := execguard.CanonicalJSON(args)
	rev := execguard.ActionRevision(
		canonical,
		entry.manifest.Version,
		string(kind),
		reqCap,
		fmt.Sprintf("%v", needsConfirm),
		dest,
	)

	caller := execguard.Caller{
		Agent:             pol.Agent,
		SessionKey:        session.SessionKeyFromCtx(ctx),
		WorkspaceScope:    session.ScopePrefixFromCtx(ctx),
		PersonalUUID:      session.PersonalUUIDFromCtx(ctx),
		ApproverPrincipal: session.ApproverPrincipalFromCtx(ctx),
		ProvenanceKind:    session.ProvenanceKindFromCtx(ctx),
		GrantedCaps:       pol.GrantedCaps,
		HostedMode:        pol.HostedMode,
	}
	if tm := session.TurnMessageFromCtx(ctx); tm != nil {
		caller.Channel = tm.Channel
		caller.ReplyTo = tm.ReplyTo
	}

	approvalID := execguard.ApprovedIDFromCtx(ctx)
	req := execguard.ActionRequest{
		CanonicalID:        canonical,
		Revision:           rev,
		Kind:               kind,
		Destination:        dest,
		Description:        spec.Description,
		RequiredCapability: reqCap,
		NeedsConfirm:       needsConfirm,
		PolicyForbidden:    forbidden,
		Args:               args,
		ArgsHash:           argsHash,
		Caller:             caller,
		ApprovalID:         approvalID,
		IdempotencyKey:     pol.IdempotencyKey,
	}

	decision, reason := execguard.Evaluate(req)
	switch decision {
	case execguard.DecisionDeny:
		return nil, nil, errors.New(reason)
	case execguard.DecisionRequireApproval:
		if approvalID == "" {
			if pol.Guard != nil {
				rec, err := pol.Guard.CreateApproval(ctx, req)
				if err != nil {
					return nil, nil, err
				}
				return nil, map[string]any{
					"ok":            false,
					"needs_confirm": true,
					"approval_id":   rec.ID,
					"confirm_id":    rec.ID,
					"tool":          canonical,
					"destination":   rec.Destination,
					"description":   rec.Description,
					"args_preview":  rec.RedactedArgs,
				}, nil
			}
			if pol.HostedMode {
				return nil, nil, errors.New("approval store is not configured")
			}
			fallbackID := uuid.New().String()
			return nil, map[string]any{
				"ok":            false,
				"needs_confirm": true,
				"approval_id":   fallbackID,
				"confirm_id":    fallbackID,
				"tool":          canonical,
				"destination":   dest,
				"description":   spec.Description,
				"args_preview":  execguard.RedactArgs(args),
			}, nil
		}
		if pol.Guard != nil {
			if _, err := pol.Guard.ClaimForExecution(ctx, approvalID, req); err != nil {
				return nil, nil, err
			}
		} else if pol.HostedMode {
			return nil, nil, errors.New("approval store is not configured")
		}
	}

	idemKey := pol.IdempotencyKey
	if idemKey == "" && approvalID != "" {
		idemKey = "approval:" + approvalID
	}
	scope := caller.WorkspaceScope
	if scope == "" {
		scope = caller.SessionKey
	}
	if pol.Guard != nil && idemKey != "" {
		rec, replayed, err := pol.Guard.BeginExecution(ctx, scope, idemKey, canonical, argsHash, dest)
		if err != nil {
			return nil, nil, err
		}
		if replayed {
			var cached any
			if rec.ResultJSON != "" {
				_ = json.Unmarshal([]byte(rec.ResultJSON), &cached)
			}
			return cached, nil, nil
		}
	}

	data, err := r.Call(execguard.WithIdempotencyKey(ctx, pol.IdempotencyKey), apiName, action, args)
	if err != nil {
		if pol.Guard != nil && idemKey != "" {
			uncertain := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
			_ = pol.Guard.CompleteExecution(context.WithoutCancel(ctx), scope, idemKey, "", err, uncertain, approvalID)
		}
		return nil, nil, err
	}
	if pol.Guard != nil && idemKey != "" {
		raw, mErr := json.Marshal(data)
		if mErr != nil {
			_ = pol.Guard.CompleteExecution(context.WithoutCancel(ctx), scope, idemKey, "", mErr, false, approvalID)
		} else {
			_ = pol.Guard.CompleteExecution(context.WithoutCancel(ctx), scope, idemKey, string(raw), nil, false, approvalID)
		}
	}
	return data, nil, nil
}

// AuthorizeAndCallBatch authorizes and executes each batch item independently.
func (r *Registry) AuthorizeAndCallBatch(ctx context.Context, calls []BatchCall, pol CallPolicy) []BatchResult {
	out := make([]BatchResult, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(idx int, c BatchCall) {
			defer wg.Done()
			itemPol := pol
			itemPol.IdempotencyKey = c.IdempotencyKey
			// Clear per-call approval ID unless explicitly scoped (batch cannot multiplex approvals).
			itemCtx := ctx
			data, needsConfirm, err := r.AuthorizeAndCall(itemCtx, c.API, c.Action, c.Args, itemPol)
			if err != nil {
				out[idx] = BatchResult{OK: false, Error: err.Error()}
				return
			}
			if needsConfirm != nil {
				out[idx] = BatchResult{
					OK:    false,
					Error: fmt.Sprintf("approval required for api:%s.%s; invoke individually via api.call", c.API, c.Action),
				}
				return
			}
			out[idx] = BatchResult{OK: true, Data: data}
		}(i, call)
	}
	wg.Wait()
	return out
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
			kind, _ := execguard.ParseActionKind(spec.Kind)
			dest := extDestination(m)
			toolReg.Register(tools.ToolSpec{
				Name:               fmt.Sprintf("api:%s.%s", pkg, act),
				Description:        spec.Description,
				Parameters:         spec.Parameters,
				NeedsConfirm:       spec.NeedsConfirm,
				RequiresCapability: reqCap,
				Kind:               kind,
				DestinationFn: func(context.Context, map[string]any) string {
					return dest
				},
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
