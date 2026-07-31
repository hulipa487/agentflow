package caps

import (
	"context"
	"encoding/json"
	"fmt"

	"agentflow/internal/core/session"
	"agentflow/internal/drivers/shell"
)

// ShellHandlers returns the per-agent shell op handler map, bound to a
// ShellManager. Follows the same factory pattern as LLMHandlers, StoreHandlers,
// and ToolHandlers.
func ShellHandlers(mgr *shell.Manager) map[string]session.OpHandler {
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
		"shell.spawn": func(ctx context.Context, op session.Op) (string, bool) {
			owner := session.OwnerFromCtx(ctx)
			if owner == "" {
				return fail(fmt.Errorf("shell.spawn: no owner in context"))
			}
			provider := op.ShellProvider
			if provider == "" {
				provider = "docker"
			}
			h, err := mgr.Spawn(ctx, owner, provider, shell.SpawnOpts{
				Image:    op.Image,
				WorkDir:  op.WorkDir,
				Env:      op.ShellEnv,
				Network:  op.Net,
				MemLimit: op.MemLimit,
				CPULimit: op.CPULimit,
				Host:     op.Host,
				User:     op.User,
				Password: op.Password,
				KeyFile:  op.KeyFile,
			})
			if err != nil {
				return fail(err)
			}
			return okJSON(map[string]any{
				"id":       h.ID,
				"provider": h.Provider,
				"state":    int(h.State),
			})
		},

		"shell.exec": func(ctx context.Context, op session.Op) (string, bool) {
			owner := session.OwnerFromCtx(ctx)
			if owner == "" {
				return fail(fmt.Errorf("shell.exec: no owner in context"))
			}
			result, err := mgr.Exec(ctx, owner, op.ShellHandle, op.Cmd)
			if err != nil {
				return fail(err)
			}
			return okJSON(result)
		},

		"shell.write": func(ctx context.Context, op session.Op) (string, bool) {
			owner := session.OwnerFromCtx(ctx)
			if owner == "" {
				return fail(fmt.Errorf("shell.write: no owner in context"))
			}
			err := mgr.Write(ctx, owner, op.ShellHandle, op.Path, []byte(op.Content))
			if err != nil {
				return fail(err)
			}
			return "true", true
		},

		"shell.destroy": func(ctx context.Context, op session.Op) (string, bool) {
			owner := session.OwnerFromCtx(ctx)
			if owner == "" {
				return fail(fmt.Errorf("shell.destroy: no owner in context"))
			}
			err := mgr.Destroy(ctx, owner, op.ShellHandle)
			if err != nil {
				return fail(err)
			}
			return "true", true
		},
	}
}
