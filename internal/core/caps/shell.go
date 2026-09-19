package caps

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"agentflow/internal/core/files"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/shell"
)

// ShellHandlers returns the per-agent shell op handler map, bound to a
// ShellManager. Follows the same factory pattern as LLMHandlers, StoreHandlers,
// and ToolHandlers. fm (the file store) powers spawn-time materialization: a
// spawn may request a project checkout into its first volume mount and/or a
// copy of the session's scratch space, both completed before the handle
// reports ready.
func ShellHandlers(mgr *shell.Manager, fm *files.Manager, agent string) map[string]session.OpHandler {
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
	scope := func(ctx context.Context) string {
		if u := session.UserUUIDFromCtx(ctx); u != "" {
			return "user:" + u
		}
		return "agent:" + agent
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
			wantCheckout := op.Project != ""
			wantScratch := op.ScratchMount != ""
			if (wantCheckout || wantScratch) && fm == nil {
				return fail(fmt.Errorf("shell.spawn: files store unavailable (disabled at boot)"))
			}
			if wantCheckout && len(op.Volumes) == 0 {
				return fail(fmt.Errorf("shell.spawn: checkout requires a volume mount (volumes = {\"host:container\"})"))
			}
			h, err := mgr.Spawn(ctx, owner, provider, shell.SpawnOpts{
				Image:     op.Image,
				WorkDir:   op.WorkDir,
				Env:       op.ShellEnv,
				Network:   op.Net,
				MemLimit:  op.MemLimit,
				CPULimit:  op.CPULimit,
				Volumes:   op.Volumes,
				Host:      op.Host,
				User:      op.User,
				Password:  op.Password,
				KeyFile:   op.KeyFile,
				ShellOpts: op.ShellOpts,
			})
			if err != nil {
				return fail(err)
			}
			// Materialize BEFORE the handle reports ready: a caller that gets a
			// handle back can exec immediately and see every file. A failure
			// mid-copy destroys the half-prepared container rather than leaving
			// it live and misleading.
			if wantCheckout {
				if err := checkoutInto(ctx, mgr, fm, h.ID, owner, scope(ctx), op.Project, op.Ref, op.Volumes[0]); err != nil {
					_ = mgr.Destroy(ctx, owner, h.ID)
					return fail(err)
				}
			}
			if wantScratch {
				if err := scratchInto(ctx, mgr, fm, h.ID, owner, op.Owner, op.ScratchMount); err != nil {
					_ = mgr.Destroy(ctx, owner, h.ID)
					return fail(err)
				}
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

// checkoutInto resolves the project's snapshot and writes every file into the
// container directory of the first volume mount. Container paths are POSIX
// regardless of the host OS.
func checkoutInto(ctx context.Context, mgr *shell.Manager, fm *files.Manager, handleID, owner, scope, project, ref, volume string) error {
	if ref == "" {
		ref = "main"
	}
	dest := volumeContainerDir(volume)
	if dest == "" {
		return fmt.Errorf("shell.spawn: volume %q has no container path", volume)
	}
	man, err := fm.Checkout(ctx, scope, project, ref)
	if err != nil {
		return fmt.Errorf("shell.spawn: checkout %s@%s: %w", project, ref, err)
	}
	for _, f := range man.Files {
		b, err := fm.ReadBlob(ctx, f.Handle, fm.MaxFileBytes())
		if err != nil {
			return fmt.Errorf("shell.spawn: read %s: %w", f.Path, err)
		}
		if err := mgr.Write(ctx, owner, handleID, path.Join(dest, f.Path), b); err != nil {
			return fmt.Errorf("shell.spawn: materialize %s: %w", f.Path, err)
		}
	}
	return nil
}

// scratchInto copies every live scratch record of the owning session into the
// given container directory.
func scratchInto(ctx context.Context, mgr *shell.Manager, fm *files.Manager, handleID, owner, sessionKey, dest string) error {
	entries, err := fm.ScratchList(ctx, sessionKey)
	if err != nil {
		return err
	}
	for _, e := range entries {
		b, err := fm.ReadBlob(ctx, e.Handle, fm.MaxFileBytes())
		if err != nil {
			return fmt.Errorf("shell.spawn: read scratch %s: %w", e.Path, err)
		}
		if err := mgr.Write(ctx, owner, handleID, path.Join(dest, e.Path), b); err != nil {
			return fmt.Errorf("shell.spawn: materialize scratch %s: %w", e.Path, err)
		}
	}
	return nil
}

// volumeContainerDir extracts the container path from a docker -v spec
// ("host:container[:ro]"). Parsing anchors on the last ":/" so a Windows host
// path ("C:\dir:/work") or a named volume ("data:/work:ro") both resolve.
func volumeContainerDir(spec string) string {
	i := strings.LastIndex(spec, ":/")
	if i < 0 {
		return ""
	}
	rest := spec[i+1:]
	if j := strings.Index(rest[1:], ":"); j >= 0 {
		rest = rest[:j+1]
	}
	return rest
}
