package caps

import (
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	"agentflow/internal/core/files"
	"agentflow/internal/core/session"
	"agentflow/internal/core/shellops"
	"agentflow/internal/drivers/shell"
)

// SSHHandlers returns the per-agent remote SSH op handler map, gated under
// capability "net.ssh".
//
// An SSH handle is a real remote connection managed by front.SSH, optionally
// defaulting host/user/auth/workdir/env from the agent's bound SSH profile
// (agents.<name>.ssh) and copying a project snapshot or session scratch files
// onto the remote host at spawn time.
func SSHHandlers(front *shellops.Front, agent string) map[string]session.OpHandler {
	requireSSH := func(opName, handleID string) error {
		if front == nil || front.SSH == nil {
			return fmt.Errorf("%s: no remote SSH support in this engine", opName)
		}
		if shellops.IsSandboxHandle(handleID) {
			return fmt.Errorf("%s: expected an SSH handle from ssh.spawn (use shell.* for sandbox handles)", opName)
		}
		return nil
	}

	return map[string]session.OpHandler{
		"ssh.spawn": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := opOwned(ctx, "ssh.spawn")
			if err != nil {
				return opFail(err)
			}
			return spawnSSH(ctx, front, agent, owner, op)
		},

		"ssh.exec": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := opOwned(ctx, "ssh.exec")
			if err != nil {
				return opFail(err)
			}
			if err := requireSSH("ssh.exec", op.ShellHandle); err != nil {
				return opFail(err)
			}
			execCtx := ctx
			if op.Timeout > 0 {
				var cancel context.CancelFunc
				execCtx, cancel = context.WithTimeout(ctx, time.Duration(op.Timeout*float64(time.Second)))
				defer cancel()
			}
			result, err := front.SSH.Exec(execCtx, owner, op.ShellHandle, op.Cmd)
			if err != nil {
				return opFail(err)
			}
			return opOKJSON(result)
		},

		"ssh.read": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := opOwned(ctx, "ssh.read")
			if err != nil {
				return opFail(err)
			}
			if err := requireSSH("ssh.read", op.ShellHandle); err != nil {
				return opFail(err)
			}
			b, err := front.SSH.Read(ctx, owner, op.ShellHandle, op.Path)
			if err != nil {
				return opFail(err)
			}
			return opOKJSON(map[string]any{"ok": true, "path": op.Path, "content": string(b)})
		},

		"ssh.write": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := opOwned(ctx, "ssh.write")
			if err != nil {
				return opFail(err)
			}
			if err := requireSSH("ssh.write", op.ShellHandle); err != nil {
				return opFail(err)
			}
			if err := front.SSH.Write(ctx, owner, op.ShellHandle, op.Path, []byte(op.Content)); err != nil {
				return opFail(err)
			}
			return "true", true
		},

		"ssh.close": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := opOwned(ctx, "ssh.close")
			if err != nil {
				return opFail(err)
			}
			if err := requireSSH("ssh.close", op.ShellHandle); err != nil {
				return opFail(err)
			}
			if err := front.SSH.Destroy(ctx, owner, op.ShellHandle); err != nil {
				return opFail(err)
			}
			return "true", true
		},
	}
}

// mergeSSHOpts combines any bound agents.<name>.ssh profile defaults from ctx
// with explicit overrides passed on ssh.spawn.
func mergeSSHOpts(ctx context.Context, op session.Op) shell.SpawnOpts {
	base, _ := shellops.SSHProfile(ctx)
	if op.WorkDir != "" {
		base.WorkDir = op.WorkDir
	}
	if op.Host != "" {
		base.Host = op.Host
	}
	if op.User != "" {
		base.User = op.User
	}
	if op.Password != "" {
		base.Password = op.Password
	}
	if op.KeyFile != "" {
		base.KeyFile = op.KeyFile
	}
	if len(op.ShellOpts) > 0 {
		base.ShellOpts = op.ShellOpts
	}
	if len(op.ShellEnv) > 0 || len(base.Env) > 0 {
		merged := make(map[string]string, len(base.Env)+len(op.ShellEnv))
		for k, v := range base.Env {
			merged[k] = v
		}
		for k, v := range op.ShellEnv {
			merged[k] = v
		}
		base.Env = merged
	}
	return base
}

// spawnSSH opens a connection to a remote machine and, when asked, copies a
// project snapshot or the session's scratch files onto it before the handle
// reports ready.
func spawnSSH(ctx context.Context, front *shellops.Front, agent, owner string, op session.Op) (string, bool) {
	if front == nil || front.SSH == nil {
		return opFail(errors.New("ssh.spawn: no remote SSH support in this engine"))
	}
	mgr, fm := front.SSH, front.Files
	wantCheckout := op.Project != ""
	wantScratch := op.ScratchMount != ""
	if (wantCheckout || wantScratch) && fm == nil {
		return opFail(fmt.Errorf("ssh.spawn: files store unavailable (disabled at boot)"))
	}
	if wantCheckout && len(op.Volumes) == 0 {
		return opFail(fmt.Errorf("ssh.spawn: checkout requires a volume mount (volumes = {\"host:container\"})"))
	}
	h, err := mgr.Spawn(ctx, owner, shellops.SSH, mergeSSHOpts(ctx, op))
	if err != nil {
		return opFail(err)
	}
	// Materialize BEFORE the handle reports ready: a caller that gets a handle
	// back can exec immediately and see every file. A failure mid-copy closes
	// the half-prepared connection rather than leaving it live and misleading.
	if wantCheckout {
		if err := checkoutInto(ctx, mgr, fm, h.ID, owner, shellops.Scope(ctx, agent), op.Project, op.Ref, op.Volumes[0]); err != nil {
			_ = mgr.Destroy(ctx, owner, h.ID)
			return opFail(err)
		}
	}
	if wantScratch {
		if err := scratchInto(ctx, mgr, fm, h.ID, owner, op.Owner, op.ScratchMount); err != nil {
			_ = mgr.Destroy(ctx, owner, h.ID)
			return opFail(err)
		}
	}
	return opOKJSON(map[string]any{"id": h.ID, "provider": h.Provider, "state": int(h.State)})
}

// checkoutInto resolves the project's snapshot and writes every file into the
// remote directory of the first volume mount. Remote paths are POSIX
// regardless of the host OS.
//
// The word "checkout" in the error below is load-bearing beyond this engine: a
// deployment's cold-start retry matches on it to tell "this project has nothing
// to check out yet" from a broken shell, and falls back to a bare shell on
// that basis. Reword it only together with a structured alternative — the test
// TestShellSpawnCheckoutFailureNamesCheckout is what will stop you.
func checkoutInto(ctx context.Context, mgr *shell.Manager, fm *files.Manager, handleID, owner, scope, project, ref, volume string) error {
	if ref == "" {
		ref = "main"
	}
	dest := volumeContainerDir(volume)
	if dest == "" {
		return fmt.Errorf("ssh.spawn: volume %q has no container path", volume)
	}
	man, err := fm.Checkout(ctx, scope, project, ref)
	if err != nil {
		return fmt.Errorf("ssh.spawn: checkout %s@%s: %w", project, ref, err)
	}
	for _, f := range man.Files {
		b, err := fm.ReadBlob(ctx, f.Handle, fm.MaxFileBytes())
		if err != nil {
			return fmt.Errorf("ssh.spawn: read %s: %w", f.Path, err)
		}
		if err := mgr.Write(ctx, owner, handleID, path.Join(dest, f.Path), b); err != nil {
			return fmt.Errorf("ssh.spawn: materialize %s: %w", f.Path, err)
		}
	}
	return nil
}

// scratchInto copies every live scratch record of the owning session into the
// given remote directory.
func scratchInto(ctx context.Context, mgr *shell.Manager, fm *files.Manager, handleID, owner, sessionKey, dest string) error {
	entries, err := fm.ScratchList(ctx, sessionKey)
	if err != nil {
		return err
	}
	for _, e := range entries {
		b, err := fm.ReadBlob(ctx, e.Handle, fm.MaxFileBytes())
		if err != nil {
			return fmt.Errorf("ssh.spawn: read scratch %s: %w", e.Path, err)
		}
		if err := mgr.Write(ctx, owner, handleID, path.Join(dest, e.Path), b); err != nil {
			return fmt.Errorf("ssh.spawn: materialize scratch %s: %w", e.Path, err)
		}
	}
	return nil
}
