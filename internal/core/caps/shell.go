package caps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentflow/internal/core/session"
	"agentflow/internal/core/shelljobs"
	"agentflow/internal/core/shellops"
	"agentflow/internal/drivers/shell"
)

func opFail(err error) (string, bool) {
	b, _ := json.Marshal(err.Error())
	return string(b), false
}

func opOKJSON(v any) (string, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return opFail(err)
	}
	return string(b), true
}

func opOwned(ctx context.Context, op string) (string, error) {
	owner := session.OwnerFromCtx(ctx)
	if owner == "" {
		return "", fmt.Errorf("%s: no owner in context", op)
	}
	return owner, nil
}

// JobDeliverer hands a background job's result to the session that started
// it, as a message in its inbox. The engine routes it like any other message,
// so it reaches the session on whichever instance owns it by then.
type JobDeliverer func(owner string, msg session.Message)

// ShellHandlers returns the per-agent local sandbox shell op handler map.
//
// A sandbox shell is not a resource: shell.run is one job, and the handle
// shell.spawn returns is only the options it was given, so that loops written
// against the old spawn/exec/destroy calls keep working. Remote SSH sessions
// live in SSHHandlers (ssh.*, gated behind net.ssh).
func ShellHandlers(front *shellops.Front, agent string, deliver JobDeliverer) map[string]session.OpHandler {
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
			if _, err := opOwned(ctx, "shell.spawn"); err != nil {
				return opFail(err)
			}
			switch op.ShellProvider {
			case shellops.SSH:
				return opFail(errors.New("shell.spawn: SSH moved to ssh.spawn (requires net.ssh capability)"))
			case "", shellops.Sandbox, "docker":
				// "docker" was the old default's name; a loop that spelled it
				// out meant "the local sandbox shell", which is what this is.
			default:
				return opFail(fmt.Errorf("shell.spawn: unknown shell provider %q (use sandbox; SSH moved to ssh.spawn)", op.ShellProvider))
			}
			o, err := options("shell.spawn", op)
			if err != nil {
				return opFail(err)
			}
			id, err := o.Encode()
			if err != nil {
				return opFail(err)
			}
			return opOKJSON(map[string]any{"id": id, "provider": shellops.Sandbox, "state": int(shell.HandleRunning)})
		},

		"shell.exec": func(ctx context.Context, op session.Op) (string, bool) {
			if _, err := opOwned(ctx, "shell.exec"); err != nil {
				return opFail(err)
			}
			if !shellops.IsSandboxHandle(op.ShellHandle) {
				return opFail(errors.New("shell.exec: expected a sandbox handle from shell.spawn (use ssh.exec for SSH handles)"))
			}
			o, err := shellops.DecodeHandle(op.ShellHandle)
			if err != nil {
				return opFail(err)
			}
			res, err := front.Run(ctx, agent, op.Cmd, o, seconds(op.Timeout))
			if err != nil {
				return opFail(err)
			}
			return opOKJSON(res)
		},

		"shell.write": func(ctx context.Context, op session.Op) (string, bool) {
			if _, err := opOwned(ctx, "shell.write"); err != nil {
				return opFail(err)
			}
			if !shellops.IsSandboxHandle(op.ShellHandle) {
				return opFail(errors.New("shell.write: expected a sandbox handle from shell.spawn (use ssh.write for SSH handles)"))
			}
			o, err := shellops.DecodeHandle(op.ShellHandle)
			if err != nil {
				return opFail(err)
			}
			if _, err := front.Write(ctx, agent, o, op.Path, []byte(op.Content)); err != nil {
				return opFail(fmt.Errorf("shell.write: %w", err))
			}
			return "true", true
		},

		"shell.destroy": func(ctx context.Context, op session.Op) (string, bool) {
			if _, err := opOwned(ctx, "shell.destroy"); err != nil {
				return opFail(err)
			}
			if !shellops.IsSandboxHandle(op.ShellHandle) {
				return opFail(errors.New("shell.destroy: expected a sandbox handle from shell.spawn (use ssh.close for SSH handles)"))
			}
			return "true", true // there was never anything to destroy
		},

		// shell.run is one command as one sandbox job, with no handle.
		"shell.run": func(ctx context.Context, op session.Op) (string, bool) {
			if _, err := opOwned(ctx, "shell.run"); err != nil {
				return opFail(err)
			}
			o, err := options("shell.run", op)
			if err != nil {
				return opFail(err)
			}
			res, err := front.Run(ctx, agent, op.Cmd, o, seconds(op.Timeout))
			if err != nil {
				return opFail(err)
			}
			return opOKJSON(res)
		},

		// shell.start queues a job and returns at once; its result arrives in
		// the session's inbox as a shell.result message.
		"shell.start": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := opOwned(ctx, "shell.start")
			if err != nil {
				return opFail(err)
			}
			o, err := options("shell.start", op)
			if err != nil {
				return opFail(err)
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
				return opFail(err)
			}
			return opOKJSON(shellops.JobMap(job))
		},

		"shell.status": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := opOwned(ctx, "shell.status")
			if err != nil {
				return opFail(err)
			}
			if front.Jobs == nil {
				return opFail(errors.New("shell.status: this engine was started without the sandbox"))
			}
			job, err := front.Jobs.Status(ctx, owner, op.JobID)
			if err != nil {
				return opFail(err)
			}
			return opOKJSON(shellops.JobMap(job))
		},

		"shell.cancel": func(ctx context.Context, op session.Op) (string, bool) {
			owner, err := opOwned(ctx, "shell.cancel")
			if err != nil {
				return opFail(err)
			}
			if front.Jobs == nil {
				return opFail(errors.New("shell.cancel: this engine was started without the sandbox"))
			}
			job, err := front.Jobs.Cancel(ctx, owner, op.JobID)
			if err != nil {
				return opFail(err)
			}
			return opOKJSON(shellops.JobMap(job))
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
