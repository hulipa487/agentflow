package safety

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
)

const (
	defaultLuaTimeout     = 200 * time.Millisecond
	defaultLuaInstrBudget = int64(100_000)
	validateLuaInstrCap   = int64(50_000)
)

var (
	luaClosedChan       = make(chan struct{})
	errLuaInstrBudget   = errors.New("lua filter instruction budget exceeded")
	disallowedLuaBaseFn = []string{
		"dofile",
		"loadfile",
		"load",
		"loadstring",
		"collectgarbage",
		"module",
		"require",
	}
)

func init() {
	close(luaClosedChan)
}

// LuaFilterOptions configures a sandboxed custom Lua 5.1 safety filter.
type LuaFilterOptions struct {
	Name        string
	Source      string
	Ingress     bool
	Egress      bool
	Sources     []string // empty matches all sources
	Timeout     time.Duration
	InstrBudget int64
	OnError     string // "pass" (default) | "block"
	Refusal     string
	Fallback    string
}

// LuaFilter runs user-authored Lua 5.1 ingress/egress hooks inside an isolated,
// deterministic, stdlib-only gopher-lua VM on each call.
type LuaFilter struct {
	opts    LuaFilterOptions
	proto   *lua.FunctionProto
	sources map[string]bool
}

type luaBudgetCtx struct {
	parent    context.Context
	max       int64
	remaining int64
	tripped   bool
	parentErr error
}

func (b *luaBudgetCtx) Deadline() (time.Time, bool) {
	if b.parent != nil {
		return b.parent.Deadline()
	}
	return time.Time{}, false
}

func (b *luaBudgetCtx) Value(key any) any {
	if b.parent != nil {
		return b.parent.Value(key)
	}
	return nil
}

func (b *luaBudgetCtx) Done() <-chan struct{} {
	if b.parentErr != nil {
		return luaClosedChan
	}
	if b.parent != nil {
		select {
		case <-b.parent.Done():
			b.parentErr = b.parent.Err()
			return luaClosedChan
		default:
		}
	}
	if b.max > 0 {
		if b.tripped {
			return luaClosedChan
		}
		b.remaining--
		if b.remaining <= 0 {
			b.tripped = true
			return luaClosedChan
		}
	}
	return nil
}

func (b *luaBudgetCtx) Err() error {
	if b.parentErr != nil {
		return b.parentErr
	}
	if b.tripped {
		return errLuaInstrBudget
	}
	if b.parent != nil {
		return b.parent.Err()
	}
	return nil
}

func compileLuaChunk(name, src string) (*lua.FunctionProto, error) {
	chunk, err := parse.Parse(strings.NewReader(src), name)
	if err != nil {
		return nil, err
	}
	return lua.Compile(chunk, name)
}

func newSandboxedLuaState(ctx context.Context, timeout time.Duration, budget int64) (*lua.LState, context.CancelFunc) {
	if timeout <= 0 {
		timeout = defaultLuaTimeout
	}
	if budget <= 0 {
		budget = defaultLuaInstrBudget
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	L := lua.NewState(lua.Options{
		SkipOpenLibs:        true,
		IncludeGoStackTrace: false,
		MinimizeStackMemory: true,
		RegistrySize:        256,
		RegistryMaxSize:     4096,
	})
	for _, open := range []func(*lua.LState) int{
		lua.OpenBase,
		lua.OpenTable,
		lua.OpenString,
		lua.OpenMath,
	} {
		L.Push(L.NewFunction(open))
		L.Push(lua.LString(""))
		L.Call(1, 0)
	}
	for _, bad := range disallowedLuaBaseFn {
		L.SetGlobal(bad, lua.LNil)
	}
	L.SetContext(&luaBudgetCtx{
		parent:    callCtx,
		max:       budget,
		remaining: budget,
	})
	return L, cancel
}

func loadProto(L *lua.LState, proto *lua.FunctionProto) error {
	fn := L.NewFunctionFromProto(proto)
	L.Push(fn)
	return L.PCall(0, 0, nil)
}

// ValidateLuaSource checks that src compiles cleanly as Lua 5.1, initializes
// without error under the sandbox budget, and defines ingress or egress.
func ValidateLuaSource(src string) error {
	if strings.TrimSpace(src) == "" {
		return errors.New("lua filter source is required")
	}
	proto, err := compileLuaChunk("safety_filter", src)
	if err != nil {
		return fmt.Errorf("syntax error: %w", err)
	}
	L, cancel := newSandboxedLuaState(context.Background(), defaultLuaTimeout, validateLuaInstrCap)
	defer cancel()
	defer L.Close()

	if err := loadProto(L, proto); err != nil {
		return fmt.Errorf("initialization error: %w", err)
	}
	_, hasIngress := L.GetGlobal("ingress").(*lua.LFunction)
	_, hasEgress := L.GetGlobal("egress").(*lua.LFunction)
	if !hasIngress && !hasEgress {
		return errors.New("lua filter must define an ingress(input) or egress(input) function")
	}
	return nil
}

// NewLuaFilter compiles and initializes a sandboxed custom Lua safety filter.
func NewLuaFilter(opts LuaFilterOptions) (*LuaFilter, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, errors.New("lua filter name is required")
	}
	if err := ValidateLuaSource(opts.Source); err != nil {
		return nil, fmt.Errorf("lua filter %q: %w", opts.Name, err)
	}
	proto, err := compileLuaChunk("lua:"+opts.Name, opts.Source)
	if err != nil {
		return nil, fmt.Errorf("lua filter %q: %w", opts.Name, err)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultLuaTimeout
	}
	if opts.InstrBudget <= 0 {
		opts.InstrBudget = defaultLuaInstrBudget
	}
	switch strings.ToLower(strings.TrimSpace(opts.OnError)) {
	case "", "pass":
		opts.OnError = "pass"
	case "block":
		opts.OnError = "block"
	default:
		return nil, fmt.Errorf("lua filter %q: on_error %q must be pass or block", opts.Name, opts.OnError)
	}
	src := map[string]bool{}
	for _, s := range opts.Sources {
		if trimmed := strings.ToLower(strings.TrimSpace(s)); trimmed != "" {
			src[trimmed] = true
		}
	}
	return &LuaFilter{opts: opts, proto: proto, sources: src}, nil
}

func (f *LuaFilter) Name() string { return "lua:" + f.opts.Name }

func (f *LuaFilter) allowsSource(source string) bool {
	if len(f.sources) == 0 {
		return true
	}
	return f.sources[strings.ToLower(strings.TrimSpace(source))]
}

func annotationsTable(L *lua.LState, src map[string]string) *lua.LTable {
	tbl := L.NewTable()
	for k, v := range src {
		tbl.RawSetString(k, lua.LString(v))
	}
	return tbl
}

func parseLuaAnnotations(tbl *lua.LTable) map[string]string {
	lv := tbl.RawGetString("annotations")
	annTbl, ok := lv.(*lua.LTable)
	if !ok {
		return nil
	}
	out := map[string]string{}
	annTbl.ForEach(func(k, v lua.LValue) {
		ks, ok := k.(lua.LString)
		if !ok || strings.TrimSpace(string(ks)) == "" {
			return
		}
		switch val := v.(type) {
		case lua.LString:
			out[string(ks)] = string(val)
		case lua.LBool:
			out[string(ks)] = strconv.FormatBool(bool(val))
		case lua.LNumber:
			out[string(ks)] = val.String()
		}
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

func luaStringField(tbl *lua.LTable, key string) (string, bool) {
	lv := tbl.RawGetString(key)
	if s, ok := lv.(lua.LString); ok {
		return string(s), true
	}
	return "", false
}

func luaBoolField(tbl *lua.LTable, key string, fallback bool) bool {
	lv := tbl.RawGetString(key)
	if lv == lua.LNil {
		return fallback
	}
	return lua.LVAsBool(lv)
}

func parseLuaDirectives(tbl *lua.LTable) []string {
	var out []string
	if single, ok := luaStringField(tbl, "directive"); ok && strings.TrimSpace(single) != "" {
		out = append(out, single)
	}
	if list, ok := tbl.RawGetString("directives").(*lua.LTable); ok {
		for i := 1; i <= list.Len(); i++ {
			if s, ok := list.RawGetInt(i).(lua.LString); ok && strings.TrimSpace(string(s)) != "" {
				out = append(out, string(s))
			}
		}
	}
	return out
}

func (f *LuaFilter) Ingress(ctx context.Context, in IngressInput) IngressResult {
	if !f.opts.Ingress || !f.allowsSource(in.Source) {
		return IngressResult{Text: in.Message}
	}
	res, err := f.runIngress(ctx, in)
	if err != nil {
		if f.opts.OnError == "block" {
			return IngressResult{
				Drop:    true,
				Skip:    true,
				Refusal: f.opts.Refusal,
				Reason:  "lua-error:" + f.opts.Name,
			}
		}
		return IngressResult{
			Text:   in.Message,
			Reason: "lua-error-pass:" + f.opts.Name,
		}
	}
	return res
}

func (f *LuaFilter) runIngress(ctx context.Context, in IngressInput) (IngressResult, error) {
	L, cancel := newSandboxedLuaState(ctx, f.opts.Timeout, f.opts.InstrBudget)
	defer cancel()
	defer L.Close()

	if err := loadProto(L, f.proto); err != nil {
		return IngressResult{}, err
	}
	fn, ok := L.GetGlobal("ingress").(*lua.LFunction)
	if !ok {
		return IngressResult{Text: in.Message}, nil
	}
	arg := L.NewTable()
	arg.RawSetString("message", lua.LString(in.Message))
	arg.RawSetString("source", lua.LString(in.Source))
	arg.RawSetString("session_id", lua.LString(in.SessionID))
	arg.RawSetString("annotations", annotationsTable(L, in.Annotations))

	if err := L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}, arg); err != nil {
		return IngressResult{}, err
	}
	ret := L.Get(-1)
	L.Pop(1)

	if ret == lua.LNil {
		return IngressResult{Text: in.Message}, nil
	}
	tbl, ok := ret.(*lua.LTable)
	if !ok {
		return IngressResult{}, fmt.Errorf("ingress must return a table or nil, got %s", ret.Type())
	}

	out := IngressResult{
		Text:        in.Message,
		Drop:        luaBoolField(tbl, "drop", false),
		Reason:      "",
		Annotations: parseLuaAnnotations(tbl),
		Directives:  parseLuaDirectives(tbl),
	}
	out.Skip = luaBoolField(tbl, "skip", out.Drop)
	if txt, hasTxt := luaStringField(tbl, "text"); hasTxt {
		out.Text = txt
	}
	if reason, ok := luaStringField(tbl, "reason"); ok {
		out.Reason = reason
	}
	if refusal, ok := luaStringField(tbl, "refusal"); ok {
		out.Refusal = refusal
	} else if out.Drop {
		out.Refusal = f.opts.Refusal
	}
	redirect, hasRedirect := luaStringField(tbl, "redirect")
	if !hasRedirect || strings.TrimSpace(redirect) == "" {
		if alt, ok := luaStringField(tbl, "model"); ok {
			redirect = alt
		}
	}
	redirect = strings.TrimSpace(redirect)
	if redirect != "" && !out.Drop && !out.Skip {
		out.ModelOverride = redirect
		out.Meta = &SafetyMeta{
			Classifier:    f.Name(),
			Action:        "redirect",
			RedirectModel: redirect,
		}
		if out.Reason == "" {
			out.Reason = "redirect:" + redirect
		}
	}
	return out, nil
}

func (f *LuaFilter) Egress(ctx context.Context, in EgressInput) EgressResult {
	if !f.opts.Egress || !f.allowsSource(in.Source) {
		return EgressResult{Text: in.Text}
	}
	res, err := f.runEgress(ctx, in)
	if err != nil {
		if f.opts.OnError == "block" {
			return EgressResult{
				Drop:     true,
				Fallback: f.opts.Fallback,
				Reason:   "lua-error:" + f.opts.Name,
			}
		}
		return EgressResult{
			Text:   in.Text,
			Reason: "lua-error-pass:" + f.opts.Name,
		}
	}
	return res
}

func (f *LuaFilter) runEgress(ctx context.Context, in EgressInput) (EgressResult, error) {
	L, cancel := newSandboxedLuaState(ctx, f.opts.Timeout, f.opts.InstrBudget)
	defer cancel()
	defer L.Close()

	if err := loadProto(L, f.proto); err != nil {
		return EgressResult{}, err
	}
	fn, ok := L.GetGlobal("egress").(*lua.LFunction)
	if !ok {
		return EgressResult{Text: in.Text}, nil
	}
	arg := L.NewTable()
	arg.RawSetString("text", lua.LString(in.Text))
	arg.RawSetString("source", lua.LString(in.Source))
	arg.RawSetString("session_id", lua.LString(in.SessionID))
	arg.RawSetString("annotations", annotationsTable(L, in.Annotations))

	if err := L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}, arg); err != nil {
		return EgressResult{}, err
	}
	ret := L.Get(-1)
	L.Pop(1)

	if ret == lua.LNil {
		return EgressResult{Text: in.Text}, nil
	}
	tbl, ok := ret.(*lua.LTable)
	if !ok {
		return EgressResult{}, fmt.Errorf("egress must return a table or nil, got %s", ret.Type())
	}

	out := EgressResult{
		Text:        in.Text,
		Drop:        luaBoolField(tbl, "drop", false),
		Annotations: parseLuaAnnotations(tbl),
	}
	if txt, hasTxt := luaStringField(tbl, "text"); hasTxt {
		out.Text = txt
	}
	if reason, ok := luaStringField(tbl, "reason"); ok {
		out.Reason = reason
	}
	if fb, ok := luaStringField(tbl, "fallback"); ok {
		out.Fallback = fb
	} else if out.Drop {
		out.Fallback = f.opts.Fallback
	}
	return out, nil
}
