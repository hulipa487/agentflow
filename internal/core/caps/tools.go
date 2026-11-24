// Package caps adapts drivers to session op handlers.
package caps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"agentflow/internal/core/execguard"
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
	Guard        *execguard.Store
	HostedMode   bool
	GrantedCaps  map[string]bool
	Agent        string
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

			t, ok := agentSet.ByName[name]
			if !ok {
				b, _ := json.Marshal(map[string]any{"ok": false, "error": fmt.Sprintf("tool %q not available", name)})
				return string(b), false
			}

			var dest string
			if t.DestinationFn != nil {
				dest = t.DestinationFn(ctx, args)
			}
			_, argsHash := execguard.CanonicalJSON(args)
			rev := execguard.ActionRevision(
				t.Name,
				string(t.Kind),
				t.RequiresCapability,
				fmt.Sprintf("%v", t.NeedsConfirm),
				t.Description,
			)

			caller := execguard.Caller{
				Agent:             w.Agent,
				SessionKey:        session.SessionKeyFromCtx(ctx),
				WorkspaceScope:    session.ScopePrefixFromCtx(ctx),
				PersonalUUID:      session.PersonalUUIDFromCtx(ctx),
				ApproverPrincipal: session.ApproverPrincipalFromCtx(ctx),
				ProvenanceKind:    session.ProvenanceKindFromCtx(ctx),
				GrantedCaps:       w.GrantedCaps,
				HostedMode:        w.HostedMode,
			}
			if tm := session.TurnMessageFromCtx(ctx); tm != nil {
				caller.Channel = tm.Channel
				caller.ReplyTo = tm.ReplyTo
			}

			reqCap := ""
			if w.GrantedCaps != nil || w.HostedMode {
				reqCap = t.RequiresCapability
			}
			approvalID := execguard.ApprovedIDFromCtx(ctx)
			req := execguard.ActionRequest{
				CanonicalID:        name,
				Revision:           rev,
				Kind:               t.Kind,
				Destination:        dest,
				Description:        t.Description,
				RequiredCapability: reqCap,
				NeedsConfirm:       t.NeedsConfirm,
				Args:               args,
				ArgsHash:           argsHash,
				Caller:             caller,
				ApprovalID:         approvalID,
				IdempotencyKey:     op.IdempotencyKey,
			}

			decision, reason := execguard.Evaluate(req)
			switch decision {
			case execguard.DecisionDeny:
				b, _ := json.Marshal(map[string]any{"ok": false, "error": reason})
				return string(b), false
			case execguard.DecisionRequireApproval:
				if approvalID == "" {
					if w.Guard != nil {
						rec, err := w.Guard.CreateApproval(ctx, req)
						if err != nil {
							b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
							return string(b), false
						}
						b, _ := json.Marshal(map[string]any{
							"ok":            false,
							"needs_confirm": true,
							"approval_id":   rec.ID,
							"confirm_id":    rec.ID,
							"tool":          name,
							"destination":   rec.Destination,
							"description":   rec.Description,
							"args_preview":  rec.RedactedArgs,
						})
						// Return ok:true so prelude passes the structured needs_confirm
						// response back to runOnce for actor.doConfirm interception.
						return string(b), true
					}
					if w.HostedMode {
						b, _ := json.Marshal(map[string]any{"ok": false, "error": "approval store is not configured"})
						return string(b), false
					}
					fallbackID := uuid.New().String()
					b, _ := json.Marshal(map[string]any{
						"ok":            false,
						"needs_confirm": true,
						"approval_id":   fallbackID,
						"confirm_id":    fallbackID,
						"tool":          name,
						"destination":   dest,
						"description":   t.Description,
						"args_preview":  execguard.RedactArgs(args),
					})
					return string(b), true
				}
				if w.Guard != nil {
					if _, err := w.Guard.ClaimForExecution(ctx, approvalID, req); err != nil {
						b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
						return string(b), false
					}
				} else if w.HostedMode {
					b, _ := json.Marshal(map[string]any{"ok": false, "error": "approval store is not configured"})
					return string(b), false
				}
			}

			idemKey := op.IdempotencyKey
			if idemKey == "" && approvalID != "" {
				idemKey = "approval:" + approvalID
			}
			scope := caller.WorkspaceScope
			if scope == "" {
				scope = caller.SessionKey
			}
			if w.Guard != nil && idemKey != "" {
				rec, replayed, err := w.Guard.BeginExecution(ctx, scope, idemKey, name, argsHash, dest)
				if err != nil {
					b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
					return string(b), false
				}
				if replayed {
					return rec.ResultJSON, true
				}
			}

			invokeCtx := execguard.WithPreAuthorized(execguard.WithIdempotencyKey(ctx, op.IdempotencyKey), name)
			res, err := t.Invoke(invokeCtx, args)
			if err != nil {
				if w.Guard != nil && idemKey != "" {
					uncertain := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
					_ = w.Guard.CompleteExecution(context.WithoutCancel(ctx), scope, idemKey, "", err, uncertain, approvalID)
				}
				b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
				return string(b), false
			}
			b, err := json.Marshal(res)
			if err != nil {
				if w.Guard != nil && idemKey != "" {
					_ = w.Guard.CompleteExecution(context.WithoutCancel(ctx), scope, idemKey, "", err, false, approvalID)
				}
				return fmt.Sprintf("%q", err.Error()), false
			}
			if w.Guard != nil && idemKey != "" {
				_ = w.Guard.CompleteExecution(context.WithoutCancel(ctx), scope, idemKey, string(b), nil, false, approvalID)
			}
			return string(b), true
		},
	}
}
