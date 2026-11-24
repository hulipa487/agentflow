package ext

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

type invokeCtxKey struct{}

type wasmInvokeState struct {
	action string
	ctx    context.Context
}

type wasmRunner struct {
	manifest Manifest
	deps     HostDeps
	rt       wazero.Runtime
	compiled wazero.CompiledModule
}

func newWasmRunner(m Manifest, baseDir string, deps HostDeps) (*wasmRunner, error) {
	entryPath, err := ResolveEntryPath(baseDir, m.Entry, baseDir != "")
	if err != nil {
		return nil, err
	}
	wasmBytes, err := os.ReadFile(entryPath)
	if err != nil {
		return nil, fmt.Errorf("api %q: read wasm %s: %w", m.Name, entryPath, err)
	}

	ctx := context.Background()
	rtCfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(m.EffectiveMemoryPages())
	rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)

	runner := &wasmRunner{
		manifest: m,
		deps:     deps,
		rt:       rt,
	}

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("api %q: instantiate WASI: %w", m.Name, err)
	}
	if err := runner.instantiateHostModule(ctx); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("api %q: instantiate host module: %w", m.Name, err)
	}
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("api %q: compile wasm: %w", m.Name, err)
	}
	runner.compiled = compiled
	return runner, nil
}

func (r *wasmRunner) Close(ctx context.Context) error {
	if r.rt != nil {
		return r.rt.Close(ctx)
	}
	return nil
}

func (r *wasmRunner) instantiateHostModule(ctx context.Context) error {
	_, err := r.rt.NewHostModuleBuilder("af_host").
		NewFunctionBuilder().
		WithFunc(func(callCtx context.Context, mod api.Module, reqPtr, reqLen uint32) uint64 {
			return r.hostHTTPRequest(callCtx, mod, reqPtr, reqLen)
		}).
		Export("http_request").
		NewFunctionBuilder().
		WithFunc(func(callCtx context.Context, mod api.Module, levelCode, msgPtr, msgLen uint32) {
			r.hostLog(callCtx, mod, levelCode, msgPtr, msgLen)
		}).
		Export("log").
		Instantiate(ctx)
	return err
}

func (r *wasmRunner) hostLog(ctx context.Context, mod api.Module, levelCode, msgPtr, msgLen uint32) {
	if mod == nil || mod.Memory() == nil {
		return
	}
	raw, ok := mod.Memory().Read(msgPtr, msgLen)
	if !ok {
		return
	}
	level := "info"
	switch levelCode {
	case 0:
		level = "debug"
	case 1:
		level = "info"
	case 2:
		level = "warn"
	case 3:
		level = "error"
	}
	action := ""
	if st, ok := ctx.Value(invokeCtxKey{}).(wasmInvokeState); ok {
		action = st.action
	}
	logFromExtension(r.deps, r.manifest.Name, action, level, string(raw))
}

func (r *wasmRunner) hostHTTPRequest(ctx context.Context, mod api.Module, reqPtr, reqLen uint32) uint64 {
	writeResp := func(payload map[string]any) uint64 {
		b, err := json.Marshal(payload)
		if err != nil {
			b = []byte(`{"ok":false,"error":"failed to encode host response"}`)
		}
		ptr, err := writeGuestBytes(ctx, mod, b)
		if err != nil {
			return 0
		}
		return (uint64(ptr) << 32) | uint64(len(b))
	}

	if mod == nil || mod.Memory() == nil {
		return 0
	}
	reqBytes, ok := mod.Memory().Read(reqPtr, reqLen)
	if !ok {
		return writeResp(map[string]any{"ok": false, "error": "http_request: out-of-bounds request buffer"})
	}

	resp, err := executeHTTP(ctx, r.manifest, r.deps, reqBytes)
	if err != nil {
		return writeResp(map[string]any{"ok": false, "error": err.Error()})
	}
	return writeResp(resp)
}

func writeGuestBytes(ctx context.Context, mod api.Module, data []byte) (uint32, error) {
	alloc := mod.ExportedFunction("alloc")
	if alloc == nil {
		return 0, fmt.Errorf("wasm module does not export alloc(size) -> ptr")
	}
	res, err := alloc.Call(ctx, uint64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("guest alloc(%d): %w", len(data), err)
	}
	if len(res) == 0 || res[0] == 0 {
		return 0, fmt.Errorf("guest alloc(%d) returned null", len(data))
	}
	ptr := uint32(res[0])
	if !mod.Memory().Write(ptr, data) {
		return 0, fmt.Errorf("out-of-bounds memory write at %d (%d bytes)", ptr, len(data))
	}
	return ptr, nil
}

type wasmInvokeRequest struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}

type wasmInvokeResponse struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

func (r *wasmRunner) Invoke(ctx context.Context, action string, args map[string]any) (any, error) {
	if _, ok := r.manifest.Actions[action]; !ok {
		return nil, fmt.Errorf("api %q has no action %q", r.manifest.Name, action)
	}
	callCtx, cancel := context.WithTimeout(ctx, r.manifest.TimeoutDuration())
	defer cancel()
	callCtx = context.WithValue(callCtx, invokeCtxKey{}, wasmInvokeState{action: action, ctx: callCtx})

	modCfg := wazero.NewModuleConfig().
		WithName("").
		WithStdout(io.Discard).
		WithStderr(io.Discard)
	mod, err := r.rt.InstantiateModule(callCtx, r.compiled, modCfg)
	if err != nil {
		return nil, fmt.Errorf("api %q instantiate: %w", r.manifest.Name, err)
	}
	defer func() { _ = mod.Close(context.Background()) }()

	if initFn := mod.ExportedFunction("_initialize"); initFn != nil {
		if _, err := initFn.Call(callCtx); err != nil {
			return nil, fmt.Errorf("api %q initialize: %w", r.manifest.Name, err)
		}
	}

	invokeFn := mod.ExportedFunction("af_invoke")
	if invokeFn == nil {
		return nil, fmt.Errorf("api %q: wasm module does not export af_invoke(ptr, len) -> u64", r.manifest.Name)
	}
	if mod.Memory() == nil {
		return nil, fmt.Errorf("api %q: wasm module has no exported memory", r.manifest.Name)
	}

	reqArg := args
	if reqArg == nil {
		reqArg = map[string]any{}
	}
	reqBytes, err := json.Marshal(wasmInvokeRequest{Action: action, Args: reqArg})
	if err != nil {
		return nil, err
	}
	reqPtr, err := writeGuestBytes(callCtx, mod, reqBytes)
	if err != nil {
		return nil, fmt.Errorf("api %q.%s: %w", r.manifest.Name, action, err)
	}
	if dealloc := mod.ExportedFunction("dealloc"); dealloc != nil {
		defer func() { _, _ = dealloc.Call(context.Background(), uint64(reqPtr), uint64(len(reqBytes))) }()
	}

	res, err := invokeFn.Call(callCtx, uint64(reqPtr), uint64(len(reqBytes)))
	if err != nil {
		return nil, fmt.Errorf("api %q.%s: %w", r.manifest.Name, action, err)
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("api %q.%s: af_invoke returned no value", r.manifest.Name, action)
	}
	packed := res[0]
	respPtr := uint32(packed >> 32)
	respLen := uint32(packed & 0xffffffff)
	if respPtr == 0 && respLen == 0 {
		return nil, nil
	}
	respBytes, ok := mod.Memory().Read(respPtr, respLen)
	if !ok {
		return nil, fmt.Errorf("api %q.%s: out-of-bounds response (%d, %d)", r.manifest.Name, action, respPtr, respLen)
	}
	if dealloc := mod.ExportedFunction("dealloc"); dealloc != nil {
		defer func() { _, _ = dealloc.Call(context.Background(), uint64(respPtr), uint64(respLen)) }()
	}

	var out wasmInvokeResponse
	if err := json.Unmarshal(respBytes, &out); err != nil {
		return nil, fmt.Errorf("api %q.%s: invalid JSON response: %w", r.manifest.Name, action, err)
	}
	if !out.OK {
		if out.Error == "" {
			out.Error = "unknown extension error"
		}
		return nil, fmt.Errorf("api %q.%s: %s", r.manifest.Name, action, out.Error)
	}
	return out.Data, nil
}
