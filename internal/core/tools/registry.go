// Package tools implements the registry and exposure pipeline for tools.
package tools

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"agentflow/internal/config"

	"github.com/google/uuid"
)

// ToolSpec describes a tool that can be exposed to the agent.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any

	// Policy overrides (default from registry; may be overridden by config).
	NeedsConfirm bool
	Permission   string
	CostLevel    int
	UserVisible  bool
	Autonomous   bool

	// Invoke runs the tool. It should return a table that serializes to JSON.
	Invoke func(ctx context.Context, args map[string]any) (any, error)
}

// JSON returns the provider-native tool definition.
func (t ToolSpec) JSON() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  NormalizeSchema(t.Parameters),
		},
	}
}

// NormalizeSchema returns a copy of a JSON-schema map with unusable `required`
// keys dropped, recursively (properties/items). An empty `required` array is
// semantically identical to an absent one, but an empty Go slice round-trips
// through the Lua sandbox as an empty table, which serializes back to JSON as
// `{}` (object) — invalid JSON Schema that strict providers (xAI) reject with
// a 400. A `required` that arrives as an object has already been mangled that
// way and is dropped here too, so no boundary can emit `"required": {}`.
func NormalizeSchema(params map[string]any) map[string]any {
	if params == nil {
		return nil
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		switch k {
		case "required":
			if badRequired(v) {
				continue
			}
			out[k] = v
		case "properties":
			m, ok := v.(map[string]any)
			if !ok {
				out[k] = v
				continue
			}
			nm := make(map[string]any, len(m))
			for pk, pv := range m {
				if sm, ok := pv.(map[string]any); ok {
					nm[pk] = NormalizeSchema(sm)
				} else {
					nm[pk] = pv
				}
			}
			out[k] = nm
		case "items":
			if sm, ok := v.(map[string]any); ok {
				out[k] = NormalizeSchema(sm)
			} else {
				out[k] = v
			}
		default:
			out[k] = v
		}
	}
	return out
}

// badRequired reports whether a schema's `required` value cannot survive the
// Lua bridge as valid JSON Schema: an empty array (becomes `{}`) or an object
// (already mangled). Non-empty arrays pass through untouched.
func badRequired(v any) bool {
	switch r := v.(type) {
	case []string:
		return len(r) == 0
	case []any:
		return len(r) == 0
	case map[string]any:
		return true
	}
	return false
}

// Registry holds all tools keyed by their full name.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]ToolSpec
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]ToolSpec{}}
}

// Register adds or replaces a tool.
func (r *Registry) Register(t ToolSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name] = t
}

// ApplyOverrides bakes the config's tool overrides (tools.policy.overrides)
// into the registry's canonical specs. Call once at boot, after every
// Register* call and before any Expose: an override naming an unregistered
// tool is a typo and fails the boot. A description override replaces the
// registered description verbatim — either a literal or the text of a
// prompts: registry key (prompts maps prompt names to resolved text); a
// reference to a key that is not in prompts fails the boot. Param overrides
// shallow-merge into parameters.properties[name]; the policy fields are
// applied here and again per-agent in Expose (same values, idempotent).
// Schemas still pass through NormalizeSchema at emit time (JSON, llm.chat),
// so an override can never produce a mangled "required": {}.
func (r *Registry) ApplyOverrides(overrides map[string]config.ToolSpecOverride, prompts map[string]string, log *slog.Logger) error {
	if len(overrides) == 0 {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	unknown := []string{}
	unresolved := []string{}
	for name, o := range overrides {
		t, ok := r.tools[name]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		if o.Description != nil {
			text, ok := o.Description.Resolve(prompts)
			if !ok {
				unresolved = append(unresolved, fmt.Sprintf("%s description -> %q", name, o.Description.Value))
				continue
			}
			t.Description = text
		}
		unresolved = append(unresolved, applyParamOverrides(&t, o.Params, prompts, log)...)
		if o.NeedsConfirm != nil {
			t.NeedsConfirm = *o.NeedsConfirm
		}
		if o.Permission != nil {
			t.Permission = *o.Permission
		}
		if o.CostLevel != nil {
			t.CostLevel = *o.CostLevel
		}
		if o.UserVisible != nil {
			t.UserVisible = *o.UserVisible
		}
		if o.Autonomous != nil {
			t.Autonomous = *o.Autonomous
		}
		r.tools[name] = t
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("tools.policy.overrides names unregistered tool(s): %s", strings.Join(unknown, ", "))
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		return fmt.Errorf("tools.policy.overrides references unknown prompt(s): %s", strings.Join(unresolved, ", "))
	}
	return nil
}

// applyParamOverrides shallow-merges param overrides into the tool schema's
// properties. Only declared params can be overridden — anything else warns
// and is skipped (a config naming a dropped param must not break the boot).
// It returns the description overrides whose prompt key does not exist, as
// "tool param -> key" descriptors.
func applyParamOverrides(t *ToolSpec, params map[string]config.ToolParamOverride, prompts map[string]string, log *slog.Logger) []string {
	if len(params) == 0 {
		return nil
	}
	props, ok := t.Parameters["properties"].(map[string]any)
	if !ok {
		log.Warn("tool override: schema declares no properties", "tool", t.Name)
		return nil
	}
	var unresolved []string
	for pname, po := range params {
		raw, declared := props[pname]
		if !declared {
			log.Warn("tool override: param not in schema, ignoring", "tool", t.Name, "param", pname)
			continue
		}
		m, ok := raw.(map[string]any)
		if !ok {
			log.Warn("tool override: param schema is not an object, ignoring", "tool", t.Name, "param", pname)
			continue
		}
		if !po.Description.IsRef && po.Description.Value == "" {
			continue
		}
		text, ok := po.Description.Resolve(prompts)
		if !ok {
			unresolved = append(unresolved, fmt.Sprintf("%s param %s -> %q", t.Name, pname, po.Description.Value))
			continue
		}
		m["description"] = text
	}
	return unresolved
}

// AgentSet is the resolved tool list for one agent.
type AgentSet struct {
	Registry *Registry
	Tools    []ToolSpec
	ByName   map[string]ToolSpec
}

// Expose computes the tools available to an agent given its skills, the global
// policy, and the execution context.
func (r *Registry) Expose(skills []string, policy config.ToolsPolicy, autonomous bool) *AgentSet {
	defaultAllow := true
	switch policy.Default {
	case "none":
		defaultAllow = false
	case "all", "":
		defaultAllow = true
	}
	forbidden := map[string]bool{}
	for _, f := range policy.Forbidden {
		forbidden[f] = true
	}

	allowed := map[string]bool{}
	if len(skills) > 0 {
		for _, s := range skills {
			allowed[s] = true
		}
	} else if !defaultAllow {
		// Default none and no skills -> nothing, regardless of what is
		// registered. (Overrides only adjust policy of exposed tools; they
		// never allow-list on their own.)
		return &AgentSet{Registry: r, Tools: []ToolSpec{}, ByName: map[string]ToolSpec{}}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	out := []ToolSpec{}
	byName := map[string]ToolSpec{}
	for name, t := range r.tools {
		if forbidden[name] {
			continue
		}
		if len(skills) > 0 && !allowed[name] {
			continue
		}
		if override, ok := policy.Overrides[name]; ok {
			if override.NeedsConfirm != nil {
				t.NeedsConfirm = *override.NeedsConfirm
			}
			if override.Permission != nil && *override.Permission == "forbidden" {
				continue
			}
			if override.CostLevel != nil {
				t.CostLevel = *override.CostLevel
			}
			if override.UserVisible != nil {
				t.UserVisible = *override.UserVisible
			}
			if override.Autonomous != nil {
				t.Autonomous = *override.Autonomous
			}
		}
		if autonomous && !t.Autonomous {
			t.NeedsConfirm = true
		}
		out = append(out, t)
		byName[name] = t
	}
	return &AgentSet{Registry: r, Tools: out, ByName: byName}
}

// Invoke runs a tool from the agent's exposed set.
func (as *AgentSet) Invoke(ctx context.Context, name string, args map[string]any) (any, error) {
	t, ok := as.ByName[name]
	if !ok {
		return nil, fmt.Errorf("tool %q is not available", name)
	}
	if t.NeedsConfirm {
		return map[string]any{
			"ok":            false,
			"needs_confirm": true,
			"tool":          name,
			"confirm_id":    uuid.New().String(),
			"description":   t.Description,
		}, nil
	}
	return t.Invoke(ctx, args)
}

// ResultUnavailable returns the honest-degradation result for an unavailable tool.
func ResultUnavailable(name string, text string) map[string]any {
	return map[string]any{
		"ok":          false,
		"unavailable": true,
		"tool":        name,
		"text":        text,
	}
}
