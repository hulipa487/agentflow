// Package caps adapts drivers to session op handlers.
package caps

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"agentflow/internal/core/session"
	"agentflow/internal/core/tools"
)

// ToolWiring carries what the tool ops need beyond one agent's exposed set:
// the overrides that target Lua-declared tools and the live prompt registry
// they resolve against. A zero value is valid — an agent with no overrides
// and no prompts behaves exactly as before.
type ToolWiring struct {
	LuaOverrides *tools.LuaOverrides
	Prompts      *session.PromptRegistry
	Log          *slog.Logger
}

// ToolHandlers returns handlers for tools.list and tools.run bound to one
// agent's exposed tool set, plus tools.overrides, which the prelude's
// tools.list consults with the names its chunk declared.
func ToolHandlers(agentSet *tools.AgentSet, w ToolWiring) map[string]session.OpHandler {
	return map[string]session.OpHandler{
		"tools.list": func(ctx context.Context, op session.Op) (string, bool) {
			if agentSet == nil {
				b, _ := json.Marshal([]tools.ToolSpec{})
				return string(b), true
			}
			defs := make([]map[string]any, 0, len(agentSet.Tools))
			for _, t := range agentSet.Tools {
				defs = append(defs, t.JSON())
			}
			b, _ := json.Marshal(defs)
			return string(b), true
		},

		// tools.declared answers a loop's declared tool names: which of them
		// this agent's skills/policy expose, and the config overrides
		// targeting them. It resolves here rather than at boot so the text is
		// current: the prompt registry is snapshotted per call, and the reload
		// watcher rewrites it from its own goroutine.
		//
		// Visibility is the same rule Expose applied to the Go tools, taken
		// from the agent's own set. The full declared-name list is reported to
		// the overrides resolver either way, so a declared-but-hidden tool
		// still counts as declared and its override is not misreported as
		// unclaimed.
		"tools.declared": func(ctx context.Context, op session.Op) (string, bool) {
			visible := map[string]bool{}
			if agentSet != nil {
				for _, name := range op.ToolNames {
					if agentSet.Visible.Allows(name) {
						visible[name] = true
					}
				}
			}
			overrides := w.LuaOverrides.For(op.ToolNames, w.Prompts.Snapshot(), w.Log)
			if overrides == nil {
				overrides = map[string]tools.ResolvedOverride{}
			}
			b, err := json.Marshal(map[string]any{
				"visible":   visible,
				"overrides": overrides,
			})
			if err != nil {
				return fmt.Sprintf("%q", err.Error()), false
			}
			return string(b), true
		},

		"tools.run": func(ctx context.Context, op session.Op) (string, bool) {
			if agentSet == nil {
				b, _ := json.Marshal(tools.ResultUnavailable("", "agent has no tools"))
				return string(b), false
			}
			name := op.Tool
			args := op.Args
			if name == "" {
				b, _ := json.Marshal(map[string]any{"ok": false, "error": "missing tool name"})
				return string(b), false
			}

			// Confirmed re-invocation: skip NeedsConfirm check.
			if op.Confirmed {
				t, ok := agentSet.ByName[name]
				if !ok {
					b, _ := json.Marshal(map[string]any{"ok": false, "error": fmt.Sprintf("tool %q not available", name)})
					return string(b), false
				}
				res, err := t.Invoke(ctx, args)
				if err != nil {
					b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
					return string(b), false
				}
				b, err := json.Marshal(res)
				if err != nil {
					return fmt.Sprintf("%q", err.Error()), false
				}
				return string(b), true
			}

			// Normal path: goes through Invoke which checks NeedsConfirm.
			res, err := agentSet.Invoke(ctx, name, args)
			if err != nil {
				b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
				return string(b), false
			}
			b, err := json.Marshal(res)
			if err != nil {
				return fmt.Sprintf("%q", err.Error()), false
			}
			// Return ok:true even if needs_confirm — the prelude does not
			// unwrap it as an error, and runOnce intercepts the flag.
			return string(b), true
		},
	}
}
