package ext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/dop251/goja"

	"agentflow/internal/script"
)

type scriptRunner struct {
	manifest Manifest
	deps     HostDeps
	prog     *goja.Program
}

func newScriptRunner(m Manifest, baseDir string, deps HostDeps) (*scriptRunner, error) {
	entryPath, err := ResolveEntryPath(baseDir, m.Entry, baseDir != "")
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(entryPath)
	if err != nil {
		return nil, fmt.Errorf("api %q: read entry %s: %w", m.Name, entryPath, err)
	}
	isTS := strings.HasSuffix(strings.ToLower(entryPath), ".ts") || strings.HasSuffix(strings.ToLower(entryPath), ".mts")
	prog, err := script.Compile(entryPath, string(raw), isTS)
	if err != nil {
		return nil, fmt.Errorf("api %q: %w", m.Name, err)
	}
	return &scriptRunner{
		manifest: m,
		deps:     deps,
		prog:     prog,
	}, nil
}

func (r *scriptRunner) Close(context.Context) error { return nil }

func (r *scriptRunner) Invoke(ctx context.Context, action string, args map[string]any) (any, error) {
	if _, ok := r.manifest.Actions[action]; !ok {
		return nil, fmt.Errorf("api %q has no action %q", r.manifest.Name, action)
	}
	timeout := r.manifest.TimeoutDuration()
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	rt := script.NewRuntime(script.RuntimeOptions{})
	if err := r.mountModuleShim(rt); err != nil {
		return nil, err
	}
	if _, err := script.RunWithTimeout(callCtx, rt, 0, func() (goja.Value, error) {
		return rt.RunProgram(r.prog)
	}); err != nil {
		return nil, fmt.Errorf("api %q init: %w", r.manifest.Name, err)
	}

	fn, found := r.resolveAction(rt, action)
	if !found {
		return nil, fmt.Errorf("api %q action %q function not exported", r.manifest.Name, action)
	}

	hostObj := r.buildHostObject(callCtx, rt, action)
	argMap := args
	if argMap == nil {
		argMap = map[string]any{}
	}

	val, err := script.RunWithTimeout(callCtx, rt, 0, func() (goja.Value, error) {
		return fn(goja.Undefined(), rt.ToValue(argMap), hostObj)
	})
	if err != nil {
		return nil, fmt.Errorf("api %q.%s: %w", r.manifest.Name, action, err)
	}
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil, nil
	}
	exported := val.Export()
	// Normalize via JSON round-trip when needed so int64/float64/maps stay clean JSON data.
	b, err := json.Marshal(exported)
	if err != nil {
		return nil, fmt.Errorf("api %q.%s: result is not JSON-serializable: %w", r.manifest.Name, action, err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *scriptRunner) mountModuleShim(rt *goja.Runtime) error {
	exports := rt.NewObject()
	module := rt.NewObject()
	_ = module.Set("exports", exports)
	if err := rt.Set("exports", exports); err != nil {
		return err
	}
	return rt.Set("module", module)
}

func (r *scriptRunner) resolveAction(rt *goja.Runtime, action string) (goja.Callable, bool) {
	if modVal := rt.Get("module"); modVal != nil && !goja.IsUndefined(modVal) {
		if modObj := modVal.ToObject(rt); modObj != nil {
			if expVal := modObj.Get("exports"); expVal != nil && !goja.IsUndefined(expVal) && !goja.IsNull(expVal) {
				expObj := expVal.ToObject(rt)
				if fn, ok := goja.AssertFunction(expObj.Get(action)); ok {
					return fn, true
				}
				if defVal := expObj.Get("default"); defVal != nil && !goja.IsUndefined(defVal) && !goja.IsNull(defVal) {
					if fn, ok := goja.AssertFunction(defVal.ToObject(rt).Get(action)); ok {
						return fn, true
					}
				}
			}
		}
	}
	if fn, ok := goja.AssertFunction(rt.Get(action)); ok {
		return fn, true
	}
	return nil, false
}

func (r *scriptRunner) buildHostObject(ctx context.Context, rt *goja.Runtime, action string) *goja.Object {
	host := rt.NewObject()

	logObj := rt.NewObject()
	for _, lvl := range []string{"debug", "info", "warn", "error"} {
		level := lvl
		_ = logObj.Set(level, func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) > 0 {
				logFromExtension(r.deps, r.manifest.Name, action, level, call.Arguments[0].String())
			}
			return goja.Undefined()
		})
	}
	_ = host.Set("log", logObj)

	httpObj := rt.NewObject()
	_ = httpObj.Set("request", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 || goja.IsUndefined(call.Arguments[0]) || goja.IsNull(call.Arguments[0]) {
			panic(rt.NewGoError(errors.New("host.http.request requires a request object")))
		}
		rawBytes, err := json.Marshal(call.Arguments[0].Export())
		if err != nil {
			panic(rt.NewGoError(fmt.Errorf("host.http.request: encode args: %w", err)))
		}
		resp, err := executeHTTP(ctx, r.manifest, r.deps, rawBytes)
		if err != nil {
			panic(rt.NewGoError(err))
		}
		return rt.ToValue(resp)
	})
	_ = host.Set("http", httpObj)

	return host
}
