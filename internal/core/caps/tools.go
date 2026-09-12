// Package caps adapts drivers to session op handlers.
package caps

import (
	"context"
	"encoding/json"
	"fmt"

	"agentflow/internal/core/session"
	"agentflow/internal/core/tools"
)

// ToolHandlers returns handlers for tools.list and tools.run bound to one
// agent's exposed tool set.
func ToolHandlers(agentSet *tools.AgentSet) map[string]session.OpHandler {
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
