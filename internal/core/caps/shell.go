package caps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"agentflow/internal/core/files"
	"agentflow/internal/core/session"
	"agentflow/internal/core/shelljobs"
	"agentflow/internal/core/shellops"
	"agentflow/internal/drivers/shell"
)

// JobDeliverer hands a background job's result to the session that started
// it, as a message in its inbox. The engine routes it like any other message,
// so it reaches the session on whichever instance owns it by then.
type JobDeliverer func(owner string, msg session.Message)

// ShellHandlers returns the per-agent shell op handler map.
//
// A sandbox shell is not a resource: shell.run is one job, and the handle
// shell.spawn returns is only the options it was given, so that loops written
// against the old spawn/exec/destroy calls keep working. An SSH shell is a
// real connection and goes through the shell manager as before, including the
// spawn-time copy of a project snapshot or the session's scratch files onto
// the remote machine.
func ShellHandlers(front *shellops.Front, agent string, deliver JobDeliverer) map[string]session.OpHandler {
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
	owned := func(ctx context.Context, op string) (string, error) {
		owner := session.OwnerFromCtx(ctx)
		if owner == "" {
			return "", fmt.Errorf("%s: no owner in context", op)
		}
		return owner, nil
	}
	seconds := func(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

	// options reads a sandbox shell's settings from an op.
	options := func(name string, op session.Op) (shellops.Options, error) {
		o := shellops.Options{Project: op.Project, Environment: op.Environment, Env: op.ShellEnv}
		if net := op.Net; net != "" && net != "none" {
			return o, fmt.Errorf("%s: network %q is not available: sandbox jobs run with no network", name, net)
		}
		if op.Ref != "" && op.Ref != "main" {
			return o, fmt.Errorf("%s: ref %q: a sandbox job runs on the project's working tree, not on a snapshot; restore the ref with files.checkout first if that is the tree you want", name, op.Ref)
		}
		if op.ScratchMount != "" {
			return o, fmt.Errorf("%s: scratch_mount is not available in a sandbox: put the files in the project instead", name)
		}
		// The old spawn mounted a host directory at a container path and
		// checked the project out there. The host half means nothing now; the
		// container half is still where the loop's commands expect the files.
		if len(op.Volumes) > 0 {
			if o.Mount = volumeContainerDir(op.Volumes[0]); o.Mount == "" {
				return o, fmt.Errorf("%s: volume %q has no container path", name, op.Volumes[0])
			}
		}
		// An image used to be anything Docker could pull. It is now the name
		// of an environment the operator declared.
		if o.Environment == "" {
			o.Environment = op.Image
		}
		if o.Environment != "" && front.Jobs != nil && !front.Jobs.HasEnvironment(o.Environment) {
			return o, fmt.Errorf("%s: %q is not a declared environment (declared: %s); a sandbox runs in an environment the deployment declares, not in an arbitrary image",
				name, o.Environment, strings.Join(front.Jobs.Environments(), ", "))
		}
		dir, err := shellops.WorkspaceDir(o.Mount, op.WorkDir)
		if err != nil {
			return o, fmt.Errorf("%s: %w", name, err)
		}
		o.WorkDir = dir
		return o, nil
	}

	return map[string]session.OpHandler{
		"shell.spawn": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := owned(ctx, "shell.spawn")
			if err != nil {
				return fail(err)
			}
			switch op.ShellProvider {
			case shellops.SSH:
				return spawnSSH(ctx, front, agent, owner, op, fail, okJSON)
			case "", shellops.Sandbox, "docker":
				// "docker" was the old default's name; a loop that spelled it
				// out meant "the local shell", which is what this is.
			default:
				return fail(fmt.Errorf("shell.spawn: unknown shell provider %q (use sandbox or ssh)", op.ShellProvider))
			}
			o, err := options("shell.spawn", op)
			if err != nil {
				return fail(err)
			}
			id, err := o.Encode()
			if err != nil {
				return fail(err)
			}
			return okJSON(map[string]any{"id": id, "provider": shellops.Sandbox, "state": int(shell.HandleRunning)})
		},

		"shell.exec": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := owned(ctx, "shell.exec")
			if err != nil {
				return fail(err)
			}
			if shellops.IsSandboxHandle(op.ShellHandle) {
				o, err := shellops.DecodeHandle(op.ShellHandle)
				if err != nil {
					return fail(err)
				}
				res, err := front.Run(ctx, agent, op.Cmd, o, seconds(op.Timeout))
				if err != nil {
					return fail(err)
				}
				return okJSON(res)
			}
			if front.SSH == nil {
				return fail(errors.New("shell.exec: no remote shell support in this engine"))
			}
			result, err := front.SSH.Exec(ctx, owner, op.ShellHandle, op.Cmd)
			if err != nil {
				return fail(err)
			}
			return okJSON(result)
		},

		"shell.write": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := owned(ctx, "shell.write")
			if err != nil {
				return fail(err)
			}
			if shellops.IsSandboxHandle(op.ShellHandle) {
				o, err := shellops.DecodeHandle(op.ShellHandle)
				if err != nil {
					return fail(err)
				}
				if _, err := front.Write(ctx, agent, o, op.Path, []byte(op.Content)); err != nil {
					return fail(fmt.Errorf("shell.write: %w", err))
				}
				return "true", true
			}
			if front.SSH == nil {
				return fail(errors.New("shell.write: no remote shell support in this engine"))
			}
			if err := front.SSH.Write(ctx, owner, op.ShellHandle, op.Path, []byte(op.Content)); err != nil {
				return fail(err)
			}
			return "true", true
		},

		"shell.destroy": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := owned(ctx, "shell.destroy")
			if err != nil {
				return fail(err)
			}
			if shellops.IsSandboxHandle(op.ShellHandle) {
				return "true", true // there was never anything to destroy
			}
			if front.SSH == nil {
				return fail(errors.New("shell.destroy: no remote shell support in this engine"))
			}
			if err := front.SSH.Destroy(ctx, owner, op.ShellHandle); err != nil {
				return fail(err)
			}
			return "true", true
		},

		// shell.run is one command as one sandbox job, with no handle.
		"shell.run": func(ctx context.Context, op session.Op) (string, bool) {
			if _, err := owned(ctx, "shell.run"); err != nil {
				return fail(err)
			}
			o, err := options("shell.run", op)
			if err != nil {
				return fail(err)
			}
			res, err := front.Run(ctx, agent, op.Cmd, o, seconds(op.Timeout))
			if err != nil {
				return fail(err)
			}
			return okJSON(res)
		},

		// shell.start queues a job and returns at once; its result arrives in
		// the session's inbox as a shell.result message.
		"shell.start": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := owned(ctx, "shell.start")
			if err != nil {
				return fail(err)
			}
			o, err := options("shell.start", op)
			if err != nil {
				return fail(err)
			}
			// The result turn runs as whoever started the job: the identity is
			// captured now, from the turn's core-stamped provenance, because
			// by the time the job ends that turn is long over.
			identity, hasIdentity := session.ActingFromCtx(ctx)
			job, err := front.Start(ctx, agent, op.Cmd, o, seconds(op.Timeout), func(j shelljobs.Job) {
				if deliver != nil {
					deliver(owner, jobMessage(owner, j, identity, hasIdentity))
				}
			})
			if err != nil {
				return fail(err)
			}
			return okJSON(shellops.JobMap(job))
		},

		"shell.status": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := owned(ctx, "shell.status")
			if err != nil {
				return fail(err)
			}
			if front.Jobs == nil {
				return fail(errors.New("shell.status: this engine was started without the sandbox"))
			}
			job, err := front.Jobs.Status(ctx, owner, op.JobID)
			if err != nil {
				return fail(err)
			}
			return okJSON(shellops.JobMap(job))
		},

		"shell.cancel": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := owned(ctx, "shell.cancel")
			if err != nil {
				return fail(err)
			}
			if front.Jobs == nil {
				return fail(errors.New("shell.cancel: this engine was started without the sandbox"))
			}
			job, err := front.Jobs.Cancel(ctx, owner, op.JobID)
			if err != nil {
				return fail(err)
			}
			return okJSON(shellops.JobMap(job))
		},
	}
}

// jobMessage is the inbox message that carries a finished background job.
//
// Kind "shell" is deliberately not a maintenance provenance, for the reason a
// timer's is not: the job was started by a tenant's turn and its result turn
// must run as that tenant, not with the all-scope access an engine-fired
// message has. It carries the tenant and membership captured at start.
func jobMessage(owner string, j shelljobs.Job, id session.ActingIdentity, hasIdentity bool) session.Message {
	prov := &session.Provenance{Kind: "shell", Principal: "session:" + owner}
	if hasIdentity {
		u := id.Personal
		prov.UserUUID = &u
		if id.Membership != "" {
			m := id.Membership
			prov.MembershipUUID = &m
		}
	}
	return session.Message{
		ID:         "shell:" + j.ID,
		Type:       "shell.result",
		From:       "system:shell",
		Ts:         time.Now().Unix(),
		Payload:    shellops.JobMap(j),
		Provenance: prov,
	}
}

// spawnSSH opens a connection to a remote machine and, when asked, copies a
// project snapshot or the session's scratch files onto it before the handle
// reports ready.
func spawnSSH(ctx context.Context, front *shellops.Front, agent, owner string, op session.Op,
	fail func(error) (string, bool), okJSON func(any) (string, bool)) (string, bool) {
	mgr, fm := front.SSH, front.Files
	if mgr == nil {
		return fail(errors.New("shell.spawn: no remote shell support in this engine"))
	}
	wantCheckout := op.Project != ""
	wantScratch := op.ScratchMount != ""
	if (wantCheckout || wantScratch) && fm == nil {
		return fail(fmt.Errorf("shell.spawn: files store unavailable (disabled at boot)"))
	}
	if wantCheckout && len(op.Volumes) == 0 {
		return fail(fmt.Errorf("shell.spawn: checkout requires a volume mount (volumes = {\"host:container\"})"))
	}
	h, err := mgr.Spawn(ctx, owner, shellops.SSH, shell.SpawnOpts{
		WorkDir:   op.WorkDir,
		Env:       op.ShellEnv,
		Host:      op.Host,
		User:      op.User,
		Password:  op.Password,
		KeyFile:   op.KeyFile,
		ShellOpts: op.ShellOpts,
	})
	if err != nil {
		return fail(err)
	}
	// Materialize BEFORE the handle reports ready: a caller that gets a handle
	// back can exec immediately and see every file. A failure mid-copy closes
	// the half-prepared connection rather than leaving it live and misleading.
	if wantCheckout {
		if err := checkoutInto(ctx, mgr, fm, h.ID, owner, shellops.Scope(ctx, agent), op.Project, op.Ref, op.Volumes[0]); err != nil {
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
	return okJSON(map[string]any{"id": h.ID, "provider": h.Provider, "state": int(h.State)})
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
// given remote directory.
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
