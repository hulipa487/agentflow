package caps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"agentflow/internal/core/ext"
	"agentflow/internal/core/session"
)

// APIHandlers exposes External API discovery and invocation ops (`api.list`,
// `api.call`, `api.batch`) gated behind the `apis` capability.
func APIHandlers(reg *ext.Registry) map[string]session.OpHandler {
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
			res, err := reg.Call(ctx, apiName, action, op.Args)
			if err != nil {
				return fail(err)
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
					API:    strings.TrimSpace(c.API),
					Action: strings.TrimSpace(c.Action),
					Args:   c.Args,
				}
			}
			return okJSON(reg.CallBatch(ctx, calls))
		},
	}
}
