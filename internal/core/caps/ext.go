package caps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"agentflow/internal/config"
	"agentflow/internal/core/execguard"
	"agentflow/internal/core/ext"
	"agentflow/internal/core/session"
)

// APIWiring configures execution-boundary policy, capabilities, and approval
// store wiring for External API ops (`api.call`, `api.batch`).
type APIWiring struct {
	Guard       *execguard.Store
	HostedMode  bool
	GrantedCaps map[string]bool
	ToolsPolicy config.ToolsPolicy
	Agent       string
}

// APIHandlers exposes External API discovery and invocation ops (`api.list`,
// `api.call`, `api.batch`) with default (nil) wiring for legacy callers.
func APIHandlers(reg *ext.Registry) map[string]session.OpHandler {
	return APIHandlersWithWiring(reg, APIWiring{})
}

// APIHandlersWithWiring exposes External API discovery and invocation ops
// (`api.list`, `api.call`, `api.batch`) enforced by w at the shared boundary.
func APIHandlersWithWiring(reg *ext.Registry, w APIWiring) map[string]session.OpHandler {
	fail := func(err error) (string, bool) {
		b, _ := json.Marshal(err.Error())
		return string(b), false
	}
	okJSON := func(v any) (string, bool) {
		b, err := json.Marshal(v)
		if err != nil {
			return fail(err)
		}
		return string(b), true
	}

	basePolicy := ext.CallPolicy{
		Guard:       w.Guard,
		HostedMode:  w.HostedMode,
		GrantedCaps: w.GrantedCaps,
		ToolsPolicy: w.ToolsPolicy,
		Agent:       w.Agent,
	}

	return map[string]session.OpHandler{
		"api.list": func(ctx context.Context, op session.Op) (string, bool) {
			if reg == nil {
				return "[]", true
			}
			return okJSON(reg.List())
		},

		"api.call": func(ctx context.Context, op session.Op) (string, bool) {
			apiName := strings.TrimSpace(op.API)
			action := strings.TrimSpace(op.Action)
			if apiName == "" {
				return fail(errors.New("api.call: api is required"))
			}
			if action == "" {
				return fail(errors.New("api.call: action is required"))
			}
			pol := basePolicy
			pol.IdempotencyKey = op.IdempotencyKey
			res, needsConfirm, err := reg.AuthorizeAndCall(ctx, apiName, action, op.Args, pol)
			if err != nil {
				return fail(err)
			}
			if needsConfirm != nil {
				return okJSON(needsConfirm)
			}
			return okJSON(res)
		},

		"api.batch": func(ctx context.Context, op session.Op) (string, bool) {
			if reg == nil {
				return fail(errors.New("external api registry is not configured"))
			}
			calls := make([]ext.BatchCall, len(op.APICalls))
			for i, c := range op.APICalls {
				calls[i] = ext.BatchCall{
					API:            strings.TrimSpace(c.API),
					Action:         strings.TrimSpace(c.Action),
					Args:           c.Args,
					IdempotencyKey: c.IdempotencyKey,
				}
			}
			return okJSON(reg.AuthorizeAndCallBatch(ctx, calls, basePolicy))
		},
	}
}
