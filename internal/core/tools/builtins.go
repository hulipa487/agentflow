// Package tools registers the built-in tools that ship with the runtime.
package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/execguard"
	"agentflow/internal/core/session"
	"agentflow/internal/core/shellops"
	"agentflow/internal/drivers/sandbox"
	"agentflow/internal/drivers/search"
)

// RegisterBuiltins adds the phase-2 builtin tools to the registry. searcher is
// the configured engine set (from config.Search); when empty the web_search
// tool reports honest-unavailable rather than failing.
func RegisterBuiltins(r *Registry, searcher *search.Set) {
	desc := "Search the public web. Returns honest unavailable if no search engine is configured."
	props := map[string]any{
		"query": map[string]any{"type": "string", "description": "Search query"},
		"count": map[string]any{"type": "number", "description": "Max results (default 10, capped by the engine)"},
	}
	if !searcher.Empty() {
		names := searcher.Names()
		props["engine"] = map[string]any{
			"type":        "string",
			"enum":        names,
			"description": fmt.Sprintf("Search engine (default %q when omitted)", searcher.Default),
		}
		desc = fmt.Sprintf(
			"Search the public web. engine selects the backend (%s); omitted uses the default (%q). Returns normalized results (title, url, summary/content, published, score).",
			strings.Join(names, " | "), searcher.Default)
	}
	r.Register(ToolSpec{
		Name:        "builtin:web_search",
		Description: desc,
		Kind:        execguard.ActionRead,
		DestinationFn: func(_ context.Context, args map[string]any) string {
			eng, _ := args["engine"].(string)
			if eng == "" {
				return "search:default"
			}
			return "search:" + eng
		},
		Parameters: map[string]any{
			"type":       "object",
			"properties": props,
			"required":   []string{"query"},
		},
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			if searcher.Empty() {
				return ResultUnavailable("builtin:web_search", "No search engine is configured."), nil
			}
			query, _ := args["query"].(string)
			req := search.Request{Query: query}
			req.Count = argCount(args["count"])

			// Check for per-engine tailored queries:
			queriesMap := map[string]string{}
			if qm, ok := args["queries"].(map[string]any); ok {
				for k, v := range qm {
					if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
						queriesMap[k] = strings.TrimSpace(s)
					}
				}
			}

			// Check for parallel multi-engine search:
			var engineList []string
			if rawEngines, ok := args["engines"].([]any); ok {
				for _, e := range rawEngines {
					if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
						engineList = append(engineList, strings.TrimSpace(s))
					}
				}
			} else if engineStr, ok := args["engine"].(string); ok && strings.Contains(engineStr, ",") {
				for _, s := range strings.Split(engineStr, ",") {
					if trimmed := strings.TrimSpace(s); trimmed != "" {
						engineList = append(engineList, trimmed)
					}
				}
			}

			// Single engine fallback if no multi-engine list provided
			if len(engineList) <= 1 {
				singleEng := ""
				if len(engineList) == 1 {
					singleEng = engineList[0]
				} else {
					singleEng, _ = args["engine"].(string)
				}
				singleReq := req
				if customQuery, ok := queriesMap[singleEng]; ok && customQuery != "" {
					singleReq.Query = customQuery
				}
				used, res, err := searcher.Search(ctx, singleEng, singleReq)
				if err != nil {
					return nil, fmt.Errorf("builtin:web_search: %w", err)
				}
				return map[string]any{
					"ok":           true,
					"tool":         "builtin:web_search",
					"engine":       used,
					"query":        res.Query,
					"count":        len(res.Results),
					"results":      res.Results,
					"time_cost_ms": res.TimeCostMs,
				}, nil
			}

			// Parallel multi-engine search: 40s timeout budget
			searchTimeout := 40 * time.Second
			if tSec := argCount(args["timeout"]); tSec > 0 {
				searchTimeout = time.Duration(tSec) * time.Second
			}
			searchCtx, searchCancel := context.WithTimeout(ctx, searchTimeout)
			defer searchCancel()

			type engineResult struct {
				engine  string
				results []search.WebResult
				err     error
			}

			resultsChan := make(chan engineResult, len(engineList)+1)
			var wg sync.WaitGroup

			for _, eng := range engineList {
				wg.Add(1)
				go func(targetEngine string) {
					defer wg.Done()
					engineReq := req
					if customQuery, ok := queriesMap[targetEngine]; ok && customQuery != "" {
						engineReq.Query = customQuery
					}
					used, res, err := searcher.Search(searchCtx, targetEngine, engineReq)
					if err != nil {
						// Fallback: if google_search fails or is unavailable, try ollama
						if targetEngine == "google_search" {
							if usedOllama, oRes, oErr := searcher.Search(searchCtx, "ollama", engineReq); oErr == nil && oRes != nil {
								resultsChan <- engineResult{engine: usedOllama, results: oRes.Results}
								return
							}
						}
						resultsChan <- engineResult{engine: targetEngine, err: err}
						return
					}
					resultsChan <- engineResult{engine: used, results: res.Results}
				}(eng)
			}

			wg.Wait()
			close(resultsChan)

			var allResults []search.WebResult
			var unresponsive []string
			var enginesUsed []string

			for r := range resultsChan {
				if r.err != nil {
					unresponsive = append(unresponsive, r.engine)
				} else {
					enginesUsed = append(enginesUsed, r.engine)
					allResults = append(allResults, r.results...)
				}
			}

			return map[string]any{
				"ok":                   true,
				"tool":                 "builtin:web_search",
				"engines":              enginesUsed,
				"unresponsive_engines": unresponsive,
				"query":                req.Query,
				"count":                len(allResults),
				"results":              allResults,
			}, nil
		},
	})
}

// argCount extracts a count that may arrive as float64 (the JSON path Lua
// values take), int, or int64; 0 when unset or non-numeric.
func argCount(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// RegisterShellBuiltins adds the model-facing shell and file tools.
//
// RegisterShellBuiltins registers builtin:shell.exec and the local project file
// tools (builtin:fs.read, builtin:fs.write).
//
// They act on the calling agent's bound local shell profile (agents.<name>.shell,
// carried in the op context), so a model never handles a handle. Each command
// runs as a one-off sandbox job on the profile's project and the file tools
// read and write that project directly. An agent without a bound profile gets
// honest-unavailable, never a raw error. Visibility is gated on the shell.exec
// capability at both registry filtering (RequiresCapability) and policy wiring
// in cmd/agentflow.
func RegisterShellBuiltins(r *Registry, front *shellops.Front) {
	registerShellProfileBuiltins(r, front)
	registerFSBuiltins(r, front)
}

// RegisterSSHBuiltins registers builtin:ssh.{exec,read,write,close}.
//
// They act on the calling agent's bound SSH profile (agents.<name>.ssh, carried
// in the op context) — lazily opening and reusing one connection per session —
// or on an explicit handle_id opened by a loop via ssh.spawn. Visibility is
// gated on the net.ssh capability at both registry filtering and policy wiring.
func RegisterSSHBuiltins(r *Registry, front *shellops.Front) {
	// sshTarget resolves either an explicit handle_id or the session's bound
	// SSH profile (opening/reusing the per-session connection when autoSpawn).
	sshTarget := func(ctx context.Context, tool string, args map[string]any, autoSpawn bool) (owner, handleID string, haveProfile bool, unavail map[string]any) {
		owner = session.OwnerFromCtx(ctx)
		if owner == "" {
			return "", "", false, ResultUnavailable(tool, "This agent has no SSH profile bound (agents.<name>.ssh).")
		}
		if front == nil || front.SSH == nil {
			return "", "", false, ResultUnavailable(tool, "This engine has no remote SSH support.")
		}
		opts, ok := shellops.SSHProfile(ctx)
		if handleID, _ = args["handle_id"].(string); handleID != "" {
			if shellops.IsSandboxHandle(handleID) {
				return "", "", ok, failed(tool, errors.New("expected an SSH handle_id (use builtin:shell.exec / builtin:fs.* for sandbox handles)"))
			}
			return owner, handleID, ok, nil
		}
		if !ok {
			return "", "", false, ResultUnavailable(tool, "This agent has no SSH profile bound (agents.<name>.ssh).")
		}
		if !autoSpawn {
			return owner, "", true, nil
		}
		h, err := front.SSH.Ensure(ctx, owner, shellops.SSH, opts)
		if err != nil {
			return owner, "", true, failed(tool, err)
		}
		return owner, h.ID, true, nil
	}

	sshDest := func(ctx context.Context, args map[string]any) string {
		if opts, ok := shellops.SSHProfile(ctx); ok && opts.Host != "" {
			return "ssh:" + opts.Host
		}
		if hid, _ := args["handle_id"].(string); hid != "" {
			return "ssh-handle:" + hid
		}
		return "ssh"
	}

	r.Register(ToolSpec{
		Name:               "builtin:ssh.exec",
		Description:        "Run a command over SSH on this agent's bound remote host (opening and reusing a per-session connection), or on an explicit SSH handle_id. Returns handle_id, stdout, stderr, exit_code and duration_ms.",
		RequiresCapability: "net.ssh",
		Kind:               execguard.ActionWrite,
		DestinationFn:      sshDest,
		Parameters: objectSchema(map[string]any{
			"command":   map[string]any{"type": "string", "description": "Command to run"},
			"handle_id": map[string]any{"type": "string", "description": "Optional: SSH handle_id from ssh.spawn instead of the bound SSH profile"},
		}, []string{"command"}),
		Permission:   "write",
		NeedsConfirm: true,
		Autonomous:   false,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			const tool = "builtin:ssh.exec"
			owner, hid, _, unavail := sshTarget(ctx, tool, args, true)
			if unavail != nil {
				return unavail, nil
			}
			cmd, _ := args["command"].(string)
			if cmd == "" {
				return map[string]any{"ok": false, "tool": tool, "error": "missing command"}, nil
			}
			res, err := front.SSH.Exec(ctx, owner, hid, cmd)
			if err != nil {
				return failed(tool, err), nil
			}
			return map[string]any{
				"ok": true, "tool": tool, "handle_id": hid,
				"stdout": res.Stdout, "stderr": res.Stderr,
				"exit_code": res.ExitCode, "duration_ms": res.Duration,
			}, nil
		},
	})

	r.Register(ToolSpec{
		Name:               "builtin:ssh.read",
		Description:        "Read a text file over SSH from this agent's bound remote host (or an explicit SSH handle_id).",
		RequiresCapability: "net.ssh",
		Kind:               execguard.ActionRead,
		DestinationFn:      sshDest,
		Parameters: objectSchema(map[string]any{
			"path":      map[string]any{"type": "string", "description": "Remote file path"},
			"handle_id": map[string]any{"type": "string", "description": "Optional: SSH handle_id from ssh.spawn instead of the bound SSH profile"},
		}, []string{"path"}),
		Permission: "read",
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			const tool = "builtin:ssh.read"
			owner, hid, _, unavail := sshTarget(ctx, tool, args, true)
			if unavail != nil {
				return unavail, nil
			}
			path, _ := args["path"].(string)
			if path == "" {
				return map[string]any{"ok": false, "tool": tool, "error": "missing path"}, nil
			}
			b, err := front.SSH.Read(ctx, owner, hid, path)
			if err != nil {
				return failed(tool, err), nil
			}
			return map[string]any{"ok": true, "tool": tool, "handle_id": hid, "path": path, "content": string(b)}, nil
		},
	})

	r.Register(ToolSpec{
		Name:               "builtin:ssh.write",
		Description:        "Write a text file over SSH on this agent's bound remote host (or an explicit SSH handle_id).",
		RequiresCapability: "net.ssh",
		Kind:               execguard.ActionWrite,
		DestinationFn:      sshDest,
		Parameters: objectSchema(map[string]any{
			"path":      map[string]any{"type": "string", "description": "Remote file path"},
			"content":   map[string]any{"type": "string", "description": "File content"},
			"handle_id": map[string]any{"type": "string", "description": "Optional: SSH handle_id from ssh.spawn instead of the bound SSH profile"},
		}, []string{"path", "content"}),
		Permission:   "write",
		NeedsConfirm: true,
		Autonomous:   false,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			const tool = "builtin:ssh.write"
			owner, hid, _, unavail := sshTarget(ctx, tool, args, true)
			if unavail != nil {
				return unavail, nil
			}
			path, _ := args["path"].(string)
			content, _ := args["content"].(string)
			if path == "" {
				return map[string]any{"ok": false, "tool": tool, "error": "missing path"}, nil
			}
			if err := front.SSH.Write(ctx, owner, hid, path, []byte(content)); err != nil {
				return failed(tool, err), nil
			}
			return map[string]any{"ok": true, "tool": tool, "handle_id": hid, "path": path}, nil
		},
	})

	r.Register(ToolSpec{
		Name:               "builtin:ssh.close",
		Description:        "Close this agent's active remote SSH connection (or a specific SSH handle_id); a later ssh call opens a fresh one.",
		RequiresCapability: "net.ssh",
		Kind:               execguard.ActionWrite,
		DestinationFn:      sshDest,
		Parameters: objectSchema(map[string]any{
			"handle_id": map[string]any{"type": "string", "description": "Optional: SSH handle_id to close; defaults to the session's bound SSH connection"},
		}, nil),
		Permission:   "write",
		NeedsConfirm: true,
		Autonomous:   false,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			const tool = "builtin:ssh.close"
			owner, hid, _, unavail := sshTarget(ctx, tool, args, false)
			if unavail != nil {
				return unavail, nil
			}
			if hid == "" {
				h := front.SSH.Current(owner)
				if h == nil {
					return map[string]any{"ok": true, "tool": tool, "closed": false}, nil
				}
				hid = h.ID
			}
			if err := front.SSH.Destroy(ctx, owner, hid); err != nil {
				return failed(tool, err), nil
			}
			return map[string]any{"ok": true, "tool": tool, "closed": true, "handle_id": hid}, nil
		},
	})
}

// bound is the calling session's local sandbox shell, resolved from the op context.
type bound struct {
	owner, agent string
}

// boundShell resolves the calling session's bound local shell profile. The
// second return value is the honest-unavailable result — non-nil exactly when
// the agent has no shell profile bound (or the context carries no owner).
func boundShell(ctx context.Context, front *shellops.Front, tool string) (bound, map[string]any) {
	owner := session.OwnerFromCtx(ctx)
	_, _, _, ok := shellops.Profile(ctx)
	if owner == "" || !ok {
		return bound{}, ResultUnavailable(tool, "This agent has no shell profile bound (agents.<name>.shell).")
	}
	if front == nil {
		return bound{}, ResultUnavailable(tool, "This engine has no shell support.")
	}
	return bound{owner: owner, agent: shellops.AgentOf(owner)}, nil
}

// failed is a tool result for something that went wrong, or the
// honest-unavailable result when the sandbox cannot run here at all.
func failed(tool string, err error) map[string]any {
	if errors.Is(err, sandbox.ErrUnavailable) {
		return ResultUnavailable(tool, err.Error())
	}
	return map[string]any{"ok": false, "tool": tool, "error": err.Error()}
}

func registerShellProfileBuiltins(r *Registry, front *shellops.Front) {
	localDest := func(ctx context.Context, _ map[string]any) string {
		if _, p, _, ok := shellops.Profile(ctx); ok && p.Project != "" {
			return "sandbox:" + p.Project
		}
		return "sandbox:workspace"
	}
	r.Register(ToolSpec{
		Name:               "builtin:shell.exec",
		Description:        "Run a shell command in this agent's project. Each call starts fresh in the project directory: files the command writes there are kept, but nothing else carries over between calls — no background processes, no shell variables, no files outside the project. Returns stdout, stderr, exit_code, duration_ms, timed_out and the project files it changed.",
		RequiresCapability: "shell.exec",
		Kind:               execguard.ActionWrite,
		DestinationFn:      localDest,
		Parameters: objectSchema(map[string]any{
			"command": map[string]any{"type": "string", "description": "Command to run"},
		}, []string{"command"}),
		Permission:   "write",
		NeedsConfirm: true,
		Autonomous:   false,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			const tool = "builtin:shell.exec"
			b, unavail := boundShell(ctx, front, tool)
			if unavail != nil {
				return unavail, nil
			}
			cmd, _ := args["command"].(string)
			if cmd == "" {
				return map[string]any{"ok": false, "error": "missing command"}, nil
			}
			res, err := front.Run(ctx, b.agent, cmd, shellops.Options{}, 0)
			if err != nil {
				return failed(tool, err), nil
			}
			res["tool"] = tool
			return res, nil
		},
	})
}

// registerFSBuiltins registers builtin:fs.read and builtin:fs.write. Both act
// on the agent's local project (and require a bound agents.<name>.shell
// profile); reading and writing files goes through the project files store
// without starting a sandbox job. Optional handle_id is restricted to a sandbox
// handle from shell.spawn so its mount/workdir options apply.
func registerFSBuiltins(r *Registry, front *shellops.Front) {
	target := func(ctx context.Context, tool string, args map[string]any) (agent string, opts shellops.Options, unavail map[string]any) {
		b, unavail := boundShell(ctx, front, tool)
		if unavail != nil {
			return "", opts, unavail
		}
		if handleID, _ := args["handle_id"].(string); handleID != "" {
			if !shellops.IsSandboxHandle(handleID) {
				return "", opts, failed(tool, errors.New("not a sandbox handle (use builtin:ssh.read / builtin:ssh.write for SSH handles)"))
			}
			o, err := shellops.DecodeHandle(handleID)
			if err != nil {
				return "", opts, failed(tool, err)
			}
			return b.agent, o, nil
		}
		return b.agent, opts, nil
	}

	localDest := func(ctx context.Context, _ map[string]any) string {
		if _, p, _, ok := shellops.Profile(ctx); ok && p.Project != "" {
			return "sandbox:" + p.Project
		}
		return "sandbox:workspace"
	}
	r.Register(ToolSpec{
		Name:               "builtin:fs.read",
		Description:        "Read a text file from this agent's project.",
		RequiresCapability: "shell.exec",
		Kind:               execguard.ActionRead,
		DestinationFn:      localDest,
		Parameters: objectSchema(map[string]any{
			"path":      map[string]any{"type": "string", "description": "File path, relative to the project"},
			"handle_id": map[string]any{"type": "string", "description": "Optional: a sandbox handle from shell.spawn whose mount and workdir options apply"},
		}, []string{"path"}),
		Permission: "read",
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			const tool = "builtin:fs.read"
			path, _ := args["path"].(string)
			agent, opts, unavail := target(ctx, tool, args)
			if unavail != nil {
				return unavail, nil
			}
			content, rel, truncated, err := front.Read(ctx, agent, opts, path)
			if err != nil {
				return failed(tool, err), nil
			}
			return map[string]any{"ok": true, "content": content, "path": rel, "truncated": truncated}, nil
		},
	})

	r.Register(ToolSpec{
		Name:               "builtin:fs.write",
		Description:        "Write a text file in this agent's project.",
		RequiresCapability: "shell.exec",
		Kind:               execguard.ActionWrite,
		DestinationFn:      localDest,
		Parameters: objectSchema(map[string]any{
			"path":      map[string]any{"type": "string", "description": "File path, relative to the project"},
			"content":   map[string]any{"type": "string", "description": "File content"},
			"handle_id": map[string]any{"type": "string", "description": "Optional: a sandbox handle from shell.spawn whose mount and workdir options apply"},
		}, []string{"path", "content"}),
		Permission:   "write",
		NeedsConfirm: true,
		Autonomous:   false,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			const tool = "builtin:fs.write"
			path, _ := args["path"].(string)
			content, _ := args["content"].(string)
			agent, opts, unavail := target(ctx, tool, args)
			if unavail != nil {
				return unavail, nil
			}
			rel, err := front.Write(ctx, agent, opts, path, []byte(content))
			if err != nil {
				return failed(tool, err), nil
			}
			return map[string]any{"ok": true, "path": rel}, nil
		},
	})
}

func objectSchema(props map[string]any, required []string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}
