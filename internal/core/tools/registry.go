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
		case "properties", "$defs", "definitions", "patternProperties", "dependentSchemas":
			// A map of names to subschemas.
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
		case "items", "prefixItems":
			// One schema, or the draft-07 tuple form: a list of them.
			switch iv := v.(type) {
			case map[string]any:
				out[k] = NormalizeSchema(iv)
			case []any:
				out[k] = normalizeSchemaList(iv)
			default:
				out[k] = v
			}
		case "allOf", "anyOf", "oneOf":
			if list, ok := v.([]any); ok {
				out[k] = normalizeSchemaList(list)
				continue
			}
			out[k] = v
		case "additionalProperties", "not", "if", "then", "else", "contains", "propertyNames":
			if sm, ok := v.(map[string]any); ok {
				out[k] = NormalizeSchema(sm)
				continue
			}
			out[k] = v
		default:
			out[k] = v
		}
	}
	return out
}

// normalizeSchemaList normalizes each schema in a list-valued keyword. A
// non-object member passes through: JSON Schema allows a boolean schema there,
// and it has nothing to normalize.
func normalizeSchemaList(list []any) []any {
	out := make([]any, len(list))
	for i, v := range list {
		if sm, ok := v.(map[string]any); ok {
			out[i] = NormalizeSchema(sm)
			continue
		}
		out[i] = v
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
// Register* call and before any Expose. A description override replaces the
// registered description verbatim — either a literal or the text of a
// prompts: registry key (prompts maps prompt names to resolved text); a
// reference to a key that is not in prompts fails the boot. Param overrides
// shallow-merge into parameters.properties[name]; the policy fields are
// applied here and again per-agent in Expose (same values, idempotent).
// Schemas still pass through NormalizeSchema at emit time (JSON, llm.chat),
// so an override can never produce a mangled "required": {}.
//
// An override naming no registered tool is not an error and is not applied
// here: it may target a Lua-declared tool, which exists only once a loop chunk
// loads, so LuaOverrides resolves it at tools.list() time. Its prompt
// references are still checked, so a misspelled prompt key fails the boot for
// either kind of tool. A declared tool that shadows a registered one takes its
// override from the same LuaOverrides entry (the two resolve to the same text).
func (r *Registry) ApplyOverrides(overrides map[string]config.ToolSpecOverride, prompts map[string]string, log *slog.Logger) error {
	if len(overrides) == 0 {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	unresolved := []string{}
	for name, o := range overrides {
		t, registered := r.tools[name]
		if !registered {
			unresolved = append(unresolved, checkPromptRefs(name, o, prompts)...)
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
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		return fmt.Errorf("tools.policy.overrides references unknown prompt(s): %s", strings.Join(unresolved, ", "))
	}
	return nil
}

// Names returns the registered tool names — what a caller needs to tell an
// override aimed at a Go tool from one aimed at a Lua-declared tool.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	return out
}

// checkPromptRefs reports an override's prompt references that do not resolve,
// without applying anything. Used for entries naming no registered tool: they
// are applied later against a Lua-declared tool, but a misspelled prompt key
// is still a boot error.
func checkPromptRefs(name string, o config.ToolSpecOverride, prompts map[string]string) []string {
	var bad []string
	if o.Description != nil && o.Description.IsRef {
		if _, ok := o.Description.Resolve(prompts); !ok {
			bad = append(bad, fmt.Sprintf("%s description -> %q", name, o.Description.Value))
		}
	}
	for pname, po := range o.Params {
		if !po.Description.IsRef {
			continue
		}
		if _, ok := po.Description.Resolve(prompts); !ok {
			bad = append(bad, fmt.Sprintf("%s param %s -> %q", name, pname, po.Description.Value))
		}
	}
	return bad
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

// ToolVisibility is the per-agent rule deciding which tool names reach the
// model: an agent's `skills` list intersected with the global
// `tools.policy.default`. It is the single source of truth for that decision,
// shared by Go-registered tools (Expose) and Lua-declared ones (the prelude's
// tools.list via the tools.declared op), so the two surfaces cannot drift.
//
// The rule: skills non-empty -> visible iff the name is listed; skills empty
// and default "none" -> nothing visible; skills empty and default all/""
// -> everything visible.
//
// It deliberately does not encode `forbidden` or the `permission: forbidden`
// override. Those gate Go tools only: a Lua-declared tool is the loop's own
// code and the Go tools policy has never applied to it.
//
// The zero value exposes nothing — an AgentSet built without one is a set with
// no visible tools, which is what a caller that never ran Expose should get.
type ToolVisibility struct {
	skills       map[string]bool
	anySkills    bool
	defaultAllow bool
}

// NewToolVisibility derives the rule from one agent's skills and the global
// tools policy.
func NewToolVisibility(skills []string, policy config.ToolsPolicy) ToolVisibility {
	// Case-insensitive so it agrees with config validation, which accepts any
	// casing of "none": comparing against the literal here would let "NONE"
	// through validation and then read as allow-all.
	v := ToolVisibility{defaultAllow: !strings.EqualFold(policy.Default, "none")}
	if len(skills) > 0 {
		v.anySkills = true
		v.skills = make(map[string]bool, len(skills))
		for _, s := range skills {
			v.skills[s] = true
		}
	}
	return v
}

// Allows reports whether a tool of this name is visible to the agent.
func (v ToolVisibility) Allows(name string) bool {
	if !v.anySkills {
		return v.defaultAllow
	}
	return v.skills[name]
}

// AgentSet is the resolved tool list for one agent.
type AgentSet struct {
	Registry *Registry
	Tools    []ToolSpec
	ByName   map[string]ToolSpec
	// Visible is the rule this set was filtered by, re-exported so the
	// Lua-declared tools can be filtered by the same one.
	Visible ToolVisibility
}

// Expose computes the tools available to an agent given its skills, the global
// policy, and the execution context.
//
// This is where `skills` and `tools.policy.default` decide visibility, and it
// governs Lua-declared tools (tool.def) exactly as it governs registered ones:
// the same ToolVisibility rule reaches the prelude through AgentSet.Visible.
// That is a behavior change for a deployment with `tools.policy.default: none`
// and no skills — a loop's declared tools were listed before and are not now,
// because before them `skills` filtered nothing on that path. `forbidden` and
// the `permission: forbidden` override still gate registered tools only.
func (r *Registry) Expose(skills []string, policy config.ToolsPolicy, autonomous bool) *AgentSet {
	vis := NewToolVisibility(skills, policy)
	forbidden := map[string]bool{}
	for _, f := range policy.Forbidden {
		forbidden[f] = true
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	out := []ToolSpec{}
	byName := map[string]ToolSpec{}
	for name, t := range r.tools {
		// Default none with no skills exposes nothing, regardless of what is
		// registered. (Overrides only adjust policy of exposed tools; they
		// never allow-list on their own.)
		if forbidden[name] || !vis.Allows(name) {
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
	return &AgentSet{Registry: r, Tools: out, ByName: byName, Visible: vis}
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
