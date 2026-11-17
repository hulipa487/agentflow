// Package tools registers the built-in tools that ship with the runtime.
package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/session"
	"agentflow/internal/core/shellops"
	"agentflow/internal/drivers/sandbox"
	"agentflow/internal/drivers/search"
	"agentflow/internal/drivers/shell"
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
// They act on the calling agent's bound shell profile (agents.<name>.shell,
// carried in the op context), so a model never handles a handle. With a
// sandbox profile each command is a one-off job on the profile's project and
// the file tools read and write that project directly; with an SSH profile
// the tools keep one connection per session, opened on first use. An agent
// without a bound profile gets honest-unavailable, never a raw error.
// Visibility is gated on the shell.exec capability at wiring time (main.go
// policy-forbids these tools for agents without it).
func RegisterShellBuiltins(r *Registry, front *shellops.Front) {
	registerShellProfileBuiltins(r, front)
	registerFSBuiltins(r, front)
}

// bound is the calling session's shell, resolved from the op context.
type bound struct {
	owner, agent string
	ssh          bool
	opts         shell.SpawnOpts // ssh only
}

// boundShell resolves the calling session's bound shell profile. The second
// return value is the honest-unavailable result — non-nil exactly when the
// agent has no shell profile bound (or the context carries no owner), in which
// case the caller returns it without doing anything.
func boundShell(ctx context.Context, tool string) (bound, map[string]any) {
	owner := session.OwnerFromCtx(ctx)
	provider, _, raw, ok := shellops.Profile(ctx)
	if owner == "" || !ok {
		return bound{}, ResultUnavailable(tool, "This agent has no shell profile bound (agents.<name>.shell).")
	}
	b := bound{owner: owner, agent: shellops.AgentOf(owner), ssh: provider == shellops.SSH}
	if b.ssh {
		str := func(key string) string {
			s, _ := raw[key].(string)
			return s
		}
		b.opts = shell.SpawnOpts{
			WorkDir: str("workdir"), Host: str("host"), User: str("user"),
			Password: str("password"), KeyFile: str("key_file"),
		}
		b.opts.Env, _ = raw["env"].(map[string]string)
	}
	return b, nil
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
	r.Register(ToolSpec{
		Name:        "builtin:shell.exec",
		Description: "Run a shell command in this agent's project. Each call starts fresh in the project directory: files the command writes there are kept, but nothing else carries over between calls — no background processes, no shell variables, no files outside the project. Returns stdout, stderr, exit_code, duration_ms, timed_out and the project files it changed.",
		Parameters: objectSchema(map[string]any{
			"command": map[string]any{"type": "string", "description": "Command to run"},
		}, []string{"command"}),
		Permission:   "write",
		NeedsConfirm: true,
		Autonomous:   false,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			const tool = "builtin:shell.exec"
			b, unavail := boundShell(ctx, tool)
			if unavail != nil {
				return unavail, nil
			}
			cmd, _ := args["command"].(string)
			if cmd == "" {
				return map[string]any{"ok": false, "error": "missing command"}, nil
			}
			if b.ssh {
				if front.SSH == nil {
					return ResultUnavailable(tool, "This engine has no remote shell support."), nil
				}
				h, err := front.SSH.Ensure(ctx, b.owner, shellops.SSH, b.opts)
				if err != nil {
					return failed(tool, err), nil
				}
				res, err := front.SSH.Exec(ctx, b.owner, h.ID, cmd)
				if err != nil {
					return failed(tool, err), nil
				}
				return map[string]any{
					"ok": true, "tool": tool, "handle_id": h.ID,
					"stdout": res.Stdout, "stderr": res.Stderr,
					"exit_code": res.ExitCode, "duration_ms": res.Duration,
				}, nil
			}
			res, err := front.Run(ctx, b.agent, cmd, shellops.Options{}, 0)
			if err != nil {
				return failed(tool, err), nil
			}
			res["tool"] = tool
			return res, nil
		},
	})

	r.Register(ToolSpec{
		Name:        "builtin:shell.write",
		Description: "Write a text file in this agent's project (or, with a remote shell profile, on the remote machine).",
		Parameters: objectSchema(map[string]any{
			"path":    map[string]any{"type": "string", "description": "File path, relative to the project"},
			"content": map[string]any{"type": "string", "description": "File content"},
		}, []string{"path", "content"}),
		Permission:   "write",
		NeedsConfirm: true,
		Autonomous:   false,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			const tool = "builtin:shell.write"
			b, unavail := boundShell(ctx, tool)
			if unavail != nil {
				return unavail, nil
			}
			path, _ := args["path"].(string)
			content, _ := args["content"].(string)
			if path == "" {
				return map[string]any{"ok": false, "error": "missing path"}, nil
			}
			if b.ssh {
				if front.SSH == nil {
					return ResultUnavailable(tool, "This engine has no remote shell support."), nil
				}
				h, err := front.SSH.Ensure(ctx, b.owner, shellops.SSH, b.opts)
				if err != nil {
					return failed(tool, err), nil
				}
				if err := front.SSH.Write(ctx, b.owner, h.ID, path, []byte(content)); err != nil {
					return failed(tool, err), nil
				}
				return map[string]any{"ok": true, "path": path, "handle_id": h.ID}, nil
			}
			rel, err := front.Write(ctx, b.agent, shellops.Options{}, path, []byte(content))
			if err != nil {
				return failed(tool, err), nil
			}
			return map[string]any{"ok": true, "path": rel}, nil
		},
	})

	r.Register(ToolSpec{
		Name:         "builtin:shell.destroy",
		Description:  "Close this agent's remote shell connection; a later shell call opens a fresh one. With a sandbox profile there is nothing to close and this does nothing.",
		Parameters:   objectSchema(map[string]any{}, nil),
		Permission:   "write",
		NeedsConfirm: true,
		Autonomous:   false,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			const tool = "builtin:shell.destroy"
			b, unavail := boundShell(ctx, tool)
			if unavail != nil {
				return unavail, nil
			}
			if !b.ssh || front.SSH == nil {
				return map[string]any{"ok": true, "destroyed": false}, nil
			}
			h := front.SSH.Current(b.owner)
			if h == nil {
				return map[string]any{"ok": true, "destroyed": false}, nil
			}
			if err := front.SSH.Destroy(ctx, b.owner, h.ID); err != nil {
				return failed(tool, err), nil
			}
			return map[string]any{"ok": true, "destroyed": true, "handle_id": h.ID}, nil
		},
	})
}

// registerFSBuiltins registers builtin:fs.read and builtin:fs.write. With a
// sandbox profile they are plain project file operations and need no sandbox:
// the project is in the files store, and reading a file does not take running
// anything. handle_id is only for a remote shell opened by a loop.
func registerFSBuiltins(r *Registry, front *shellops.Front) {
	// target decides where a file call goes: a remote handle, or the project.
	target := func(ctx context.Context, tool string, args map[string]any) (owner, agent, handleID string, opts shellops.Options, remote bool, unavail map[string]any) {
		owner = session.OwnerFromCtx(ctx)
		agent = shellops.AgentOf(owner)
		handleID, _ = args["handle_id"].(string)
		if handleID != "" && !shellops.IsSandboxHandle(handleID) {
			// A remote handle a loop opened: the shell manager checks that this
			// session owns it.
			return owner, agent, handleID, opts, true, nil
		}
		// Everything else acts on the agent's project, and that takes a bound
		// shell profile — the same condition as running a command. A sandbox
		// handle is only options, and a model can write one out by hand: it
		// must never be a way to reach a project without a shell.
		b, unavail := boundShell(ctx, tool)
		if unavail != nil {
			return owner, agent, "", opts, false, unavail
		}
		if handleID != "" {
			if b.ssh {
				return owner, agent, "", opts, false, failed(tool, errors.New("this agent's shell is a remote connection; a sandbox handle does not apply to it"))
			}
			o, err := shellops.DecodeHandle(handleID)
			if err != nil {
				return owner, agent, handleID, opts, false, failed(tool, err)
			}
			return owner, agent, handleID, o, false, nil
		}
		if b.ssh && front.SSH != nil {
			if h := front.SSH.Current(b.owner); h != nil {
				return owner, agent, h.ID, opts, true, nil
			}
			return owner, agent, "", opts, false, ResultUnavailable(tool, "No remote shell is open yet; run a command first.")
		}
		return owner, agent, "", opts, false, nil
	}

	r.Register(ToolSpec{
		Name:        "builtin:fs.read",
		Description: "Read a text file from this agent's project.",
		Parameters: objectSchema(map[string]any{
			"path":      map[string]any{"type": "string", "description": "File path, relative to the project"},
			"handle_id": map[string]any{"type": "string", "description": "Optional: a shell handle to read from instead"},
		}, []string{"path"}),
		Permission: "read",
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			const tool = "builtin:fs.read"
			path, _ := args["path"].(string)
			owner, agent, handleID, opts, remote, unavail := target(ctx, tool, args)
			if unavail != nil {
				return unavail, nil
			}
			if remote {
				if front.SSH == nil {
					return ResultUnavailable(tool, "This engine has no remote shell support."), nil
				}
				b, err := front.SSH.Read(ctx, owner, handleID, path)
				if err != nil {
					return failed(tool, err), nil
				}
				return map[string]any{"ok": true, "content": string(b), "path": path}, nil
			}
			content, rel, truncated, err := front.Read(ctx, agent, opts, path)
			if err != nil {
				return failed(tool, err), nil
			}
			return map[string]any{"ok": true, "content": content, "path": rel, "truncated": truncated}, nil
		},
	})

	r.Register(ToolSpec{
		Name:        "builtin:fs.write",
		Description: "Write a text file in this agent's project.",
		Parameters: objectSchema(map[string]any{
			"path":      map[string]any{"type": "string", "description": "File path, relative to the project"},
			"content":   map[string]any{"type": "string", "description": "File content"},
			"handle_id": map[string]any{"type": "string", "description": "Optional: a shell handle to write to instead"},
		}, []string{"path", "content"}),
		Permission:   "write",
		NeedsConfirm: true,
		Autonomous:   false,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			const tool = "builtin:fs.write"
			path, _ := args["path"].(string)
			content, _ := args["content"].(string)
			owner, agent, handleID, opts, remote, unavail := target(ctx, tool, args)
			if unavail != nil {
				return unavail, nil
			}
			if remote {
				if front.SSH == nil {
					return ResultUnavailable(tool, "This engine has no remote shell support."), nil
				}
				if err := front.SSH.Write(ctx, owner, handleID, path, []byte(content)); err != nil {
					return failed(tool, err), nil
				}
				return map[string]any{"ok": true, "path": path}, nil
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
