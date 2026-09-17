package tools

import (
	"log/slog"
	"sync"

	"agentflow/internal/config"
)

// LuaOverrides holds the tools.policy.overrides entries that can reach a
// Lua-declared tool. A declared tool never enters the Go registry — it exists
// only once a loop chunk loads — so its override cannot be baked at boot the
// way a registered tool's is. These entries are held here and resolved per
// call by the tools.overrides op, which the prelude's tools.list consults with
// the names its chunk declared.
//
// Registered tool names are held too, not just unknown ones: a loop may
// declare a tool that shadows a Go tool, and the winner still has to carry the
// override. Both resolve to the same text, since both read the same config
// entry.
//
// Resolution happens at the call, against the prompt registry's current text,
// so editing a file:-backed prompt is reflected in the next tools.list()
// without a restart.
//
// A name that is neither a registered Go tool nor declared by any loaded loop
// is a misspelling that would otherwise do nothing at all. Boot cannot detect
// it — Lua tool names do not exist until a chunk runs — so it is reported
// here instead: once per name, the first time a loop reports its declared
// tools without claiming it. With several agents declaring different tool
// sets, the line may be raised by an agent that does not declare a tool
// another agent does; the message names the override key, so it is traceable
// either way.
type LuaOverrides struct {
	specs      map[string]config.ToolSpecOverride
	registered map[string]bool

	mu       sync.Mutex
	declared map[string]bool // names some loaded loop has declared
	reported map[string]bool // names already warned about
}

// NewLuaOverrides copies the override entries and the set of names that are
// registered Go tools. It returns nil when there are no overrides, and every
// method tolerates a nil receiver.
func NewLuaOverrides(overrides map[string]config.ToolSpecOverride, registered []string) *LuaOverrides {
	if len(overrides) == 0 {
		return nil
	}
	cp := make(map[string]config.ToolSpecOverride, len(overrides))
	for k, v := range overrides {
		cp[k] = v
	}
	reg := make(map[string]bool, len(registered))
	for _, n := range registered {
		reg[n] = true
	}
	return &LuaOverrides{
		specs:      cp,
		registered: reg,
		declared:   map[string]bool{},
		reported:   map[string]bool{},
	}
}

// ResolvedOverride is one declared tool's override as the loop consumes it:
// the model-facing text only, every {prompt: key} already resolved. Schema and
// handler stay with tool.def — config moves presentation, never behavior.
type ResolvedOverride struct {
	Description *string                  `json:"description,omitempty"`
	Params      map[string]ResolvedParam `json:"params,omitempty"`
}

// ResolvedParam is one parameter's overridden description.
type ResolvedParam struct {
	Description string `json:"description"`
}

// For returns the overrides applying to the named declared tools, with prompt
// references resolved against prompts (a snapshot of the live registry). It
// also records the names as declared and reports any deferred override that no
// loop has claimed. The result is nil when nothing applies.
func (l *LuaOverrides) For(names []string, prompts map[string]string, log *slog.Logger) map[string]ResolvedOverride {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	out := map[string]ResolvedOverride{}
	for _, name := range names {
		l.declared[name] = true
		o, ok := l.specs[name]
		if !ok {
			continue
		}
		out[name] = resolveOverride(o, prompts)
	}
	if log != nil {
		for name := range l.specs {
			if l.registered[name] || l.declared[name] || l.reported[name] {
				continue
			}
			l.reported[name] = true
			log.Warn("tools.policy.overrides: tool is neither registered nor declared by a loaded loop",
				"tool", name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveOverride applies the same field rules the Go path uses: a description
// replaces the declared text, and a param description refines only a param the
// schema declares (the prelude skips the rest).
func resolveOverride(o config.ToolSpecOverride, prompts map[string]string) ResolvedOverride {
	var ro ResolvedOverride
	if o.Description != nil {
		if text, ok := o.Description.Resolve(prompts); ok {
			ro.Description = &text
		}
	}
	if len(o.Params) > 0 {
		ro.Params = map[string]ResolvedParam{}
		for pname, po := range o.Params {
			if !po.Description.IsRef && po.Description.Value == "" {
				continue
			}
			if text, ok := po.Description.Resolve(prompts); ok {
				ro.Params[pname] = ResolvedParam{Description: text}
			}
		}
	}
	return ro
}
