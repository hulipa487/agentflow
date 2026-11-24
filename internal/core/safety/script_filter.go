package safety

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dop251/goja"

	"agentflow/internal/script"
)

const (
	defaultScriptTimeout     = 200 * time.Millisecond
	defaultScriptInstrBudget = int64(100_000)
)

// ScriptFilterOptions configures a sandboxed TypeScript/JS safety filter.
type ScriptFilterOptions struct {
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

// ScriptFilter runs user-authored TypeScript/JS ingress/egress hooks inside an
// isolated, deterministic, pure-ES Goja VM on each call.
type ScriptFilter struct {
	opts    ScriptFilterOptions
	prog    *goja.Program
	sources map[string]bool
}

func effectiveTimeout(timeout time.Duration, instrBudget int64) time.Duration {
	if timeout <= 0 {
		timeout = defaultScriptTimeout
	}
	if instrBudget > 0 {
		bDur := time.Duration(instrBudget) * 5 * time.Microsecond
		if bDur < 25*time.Millisecond {
			bDur = 25 * time.Millisecond
		}
		if bDur < timeout {
			timeout = bDur
		}
	}
	return timeout
}

func newSandboxedScriptRuntime() *goja.Runtime {
	rt := script.NewRuntime(script.RuntimeOptions{})
	_ = rt.Set("exports", rt.NewObject())
	return rt
}

func lookupScriptHook(rt *goja.Runtime, name string) (goja.Callable, bool) {
	if fn, ok := goja.AssertFunction(rt.Get(name)); ok {
		return fn, true
	}
	if exp := rt.Get("exports"); exp != nil && !goja.IsUndefined(exp) && !goja.IsNull(exp) {
		obj := exp.ToObject(rt)
		if fn, ok := goja.AssertFunction(obj.Get(name)); ok {
			return fn, true
		}
	}
	return nil, false
}

// ValidateScriptSource checks that src strips TypeScript and compiles cleanly
// under Goja, initializes without error under the sandbox timeout, and defines
// an ingress or egress function.
func ValidateScriptSource(src string) error {
	if strings.TrimSpace(src) == "" {
		return errors.New("script filter source is required")
	}
	prog, err := script.Compile("safety_filter.ts", src, true)
	if err != nil {
		return fmt.Errorf("syntax error: %w", err)
	}
	rt := newSandboxedScriptRuntime()
	_, err = script.RunWithTimeout(context.Background(), rt, defaultScriptTimeout, func() (goja.Value, error) {
		return rt.RunProgram(prog)
	})
	if err != nil {
		return fmt.Errorf("initialization error: %w", err)
	}
	_, hasIngress := lookupScriptHook(rt, "ingress")
	_, hasEgress := lookupScriptHook(rt, "egress")
	if !hasIngress && !hasEgress {
		return errors.New("script filter must define an ingress(input) or egress(input) function")
	}
	return nil
}

// NewScriptFilter compiles and initializes a sandboxed custom TypeScript/JS safety filter.
func NewScriptFilter(opts ScriptFilterOptions) (*ScriptFilter, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, errors.New("script filter name is required")
	}
	if err := ValidateScriptSource(opts.Source); err != nil {
		return nil, fmt.Errorf("script filter %q: %w", opts.Name, err)
	}
	prog, err := script.Compile("script:"+opts.Name+".ts", opts.Source, true)
	if err != nil {
		return nil, fmt.Errorf("script filter %q: %w", opts.Name, err)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultScriptTimeout
	}
	if opts.InstrBudget <= 0 {
		opts.InstrBudget = defaultScriptInstrBudget
	}
	switch strings.ToLower(strings.TrimSpace(opts.OnError)) {
	case "", "pass":
		opts.OnError = "pass"
	case "block":
		opts.OnError = "block"
	default:
		return nil, fmt.Errorf("script filter %q: on_error %q must be pass or block", opts.Name, opts.OnError)
	}
	src := map[string]bool{}
	for _, s := range opts.Sources {
		if trimmed := strings.ToLower(strings.TrimSpace(s)); trimmed != "" {
			src[trimmed] = true
		}
	}
	return &ScriptFilter{opts: opts, prog: prog, sources: src}, nil
}

func (f *ScriptFilter) Name() string { return "script:" + f.opts.Name }

func (f *ScriptFilter) allowsSource(source string) bool {
	if len(f.sources) == 0 {
		return true
	}
	return f.sources[strings.ToLower(strings.TrimSpace(source))]
}

func copyAnnotationsMap(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func parseScriptAnnotations(tbl map[string]any) map[string]string {
	raw, ok := tbl["annotations"]
	if !ok || raw == nil {
		return nil
	}
	annMap, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for k, v := range annMap {
		if strings.TrimSpace(k) == "" {
			continue
		}
		switch val := v.(type) {
		case string:
			out[k] = val
		case bool:
			out[k] = strconv.FormatBool(val)
		case int64:
			out[k] = strconv.FormatInt(val, 10)
		case int:
			out[k] = strconv.Itoa(val)
		case float64:
			out[k] = strconv.FormatFloat(val, 'g', -1, 64)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mapStringField(tbl map[string]any, key string) (string, bool) {
	v, ok := tbl[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func mapBoolField(tbl map[string]any, key string, fallback bool) bool {
	v, ok := tbl[key]
	if !ok || v == nil {
		return fallback
	}
	switch tv := v.(type) {
	case bool:
		return tv
	case string:
		return tv != ""
	case int64:
		return tv != 0
	case int:
		return tv != 0
	case float64:
		return tv != 0
	default:
		return true
	}
}

func parseScriptDirectives(tbl map[string]any) []string {
	var out []string
	if single, ok := mapStringField(tbl, "directive"); ok && strings.TrimSpace(single) != "" {
		out = append(out, single)
	}
	if rawList, ok := tbl["directives"].([]any); ok {
		for _, item := range rawList {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func (f *ScriptFilter) callHook(ctx context.Context, hookName string, input map[string]any) (map[string]any, error) {
	rt := newSandboxedScriptRuntime()
	timeout := effectiveTimeout(f.opts.Timeout, f.opts.InstrBudget)
	val, err := script.RunWithTimeout(ctx, rt, timeout, func() (goja.Value, error) {
		if _, err := rt.RunProgram(f.prog); err != nil {
			return nil, err
		}
		fn, ok := lookupScriptHook(rt, hookName)
		if !ok {
			return goja.Undefined(), nil
		}
		return fn(goja.Undefined(), rt.ToValue(input))
	})
	if err != nil {
		return nil, err
	}
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil, nil
	}
	tbl, ok := val.Export().(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must return an object or null/undefined, got %T", hookName, val.Export())
	}
	return tbl, nil
}

func (f *ScriptFilter) Ingress(ctx context.Context, in IngressInput) IngressResult {
	if !f.opts.Ingress || !f.allowsSource(in.Source) {
		return IngressResult{Text: in.Message}
	}
	input := map[string]any{
		"message":     in.Message,
		"source":      in.Source,
		"session_id":  in.SessionID,
		"annotations": copyAnnotationsMap(in.Annotations),
	}
	tbl, err := f.callHook(ctx, "ingress", input)
	if err != nil {
		if f.opts.OnError == "block" {
			return IngressResult{
				Drop:    true,
				Skip:    true,
				Refusal: f.opts.Refusal,
				Reason:  "script-error:" + f.opts.Name,
			}
		}
		return IngressResult{
			Text:   in.Message,
			Reason: "script-error-pass:" + f.opts.Name,
		}
	}
	if tbl == nil {
		return IngressResult{Text: in.Message}
	}

	out := IngressResult{
		Text:        in.Message,
		Drop:        mapBoolField(tbl, "drop", false),
		Annotations: parseScriptAnnotations(tbl),
		Directives:  parseScriptDirectives(tbl),
	}
	out.Skip = mapBoolField(tbl, "skip", out.Drop)
	if txt, hasTxt := mapStringField(tbl, "text"); hasTxt {
		out.Text = txt
	}
	if reason, ok := mapStringField(tbl, "reason"); ok {
		out.Reason = reason
	}
	if refusal, ok := mapStringField(tbl, "refusal"); ok {
		out.Refusal = refusal
	} else if out.Drop {
		out.Refusal = f.opts.Refusal
	}
	redirect, hasRedirect := mapStringField(tbl, "redirect")
	if !hasRedirect || strings.TrimSpace(redirect) == "" {
		if alt, ok := mapStringField(tbl, "model"); ok {
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
	return out
}

func (f *ScriptFilter) Egress(ctx context.Context, in EgressInput) EgressResult {
	if !f.opts.Egress || !f.allowsSource(in.Source) {
		return EgressResult{Text: in.Text}
	}
	input := map[string]any{
		"text":        in.Text,
		"source":      in.Source,
		"session_id":  in.SessionID,
		"annotations": copyAnnotationsMap(in.Annotations),
	}
	tbl, err := f.callHook(ctx, "egress", input)
	if err != nil {
		if f.opts.OnError == "block" {
			return EgressResult{
				Drop:     true,
				Fallback: f.opts.Fallback,
				Reason:   "script-error:" + f.opts.Name,
			}
		}
		return EgressResult{
			Text:   in.Text,
			Reason: "script-error-pass:" + f.opts.Name,
		}
	}
	if tbl == nil {
		return EgressResult{Text: in.Text}
	}

	out := EgressResult{
		Text:        in.Text,
		Drop:        mapBoolField(tbl, "drop", false),
		Annotations: parseScriptAnnotations(tbl),
	}
	if txt, hasTxt := mapStringField(tbl, "text"); hasTxt {
		out.Text = txt
	}
	if reason, ok := mapStringField(tbl, "reason"); ok {
		out.Reason = reason
	}
	if fb, ok := mapStringField(tbl, "fallback"); ok {
		out.Fallback = fb
	} else if out.Drop {
		out.Fallback = f.opts.Fallback
	}
	return out
}
