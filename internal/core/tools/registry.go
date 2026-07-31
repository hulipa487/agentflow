// Package tools implements the registry and exposure pipeline for tools.
package tools

import (
	"context"
	"fmt"
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
		"type":        "function",
		"function": map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
		},
	}
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
	} else if defaultAllow {
		// All tools are allowed.
	} else {
		// Default none and no skills -> nothing.
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
