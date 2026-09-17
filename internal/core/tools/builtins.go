// Package tools registers the built-in tools that ship with the runtime.
package tools

import (
	"context"
	"fmt"
	"strings"

	"agentflow/internal/core/session"
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
			engine, _ := args["engine"].(string)
			used, res, err := searcher.Search(ctx, engine, req)
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

// RegisterShellBuiltins adds tools that operate inside shell handles.
func RegisterShellBuiltins(r *Registry, mgr *shell.Manager) {
	registerFSBuiltins(r, mgr)
	registerShellProfileBuiltins(r, mgr)
}

// registerShellProfileBuiltins registers builtin:shell.exec / shell.write /
// shell.destroy — model-facing tools over the shell driver that resolve the
// calling agent's bound shell profile (agents.<name>.shell, carried in the
// op context) instead of taking a handle id: the first call lazily spawns a
// handle, later calls reuse it, and builtin:shell.destroy tears it down. An
// agent without a bound profile gets honest-unavailable, never a raw error.
// Visibility is gated on the shell.exec capability at wiring time (main.go
// policy-forbids these tools for agents without it).
func registerShellProfileBuiltins(r *Registry, mgr *shell.Manager) {
	r.Register(ToolSpec{
		Name:        "builtin:shell.exec",
		Description: "Run a command in this agent's shell. The shell is spawned lazily from the agent's bound shell profile on first use and reused after, so files and state persist between calls. Returns stdout, stderr, exit_code, duration_ms.",
		Parameters: objectSchema(map[string]any{
			"command": map[string]any{"type": "string", "description": "Command to run"},
		}, []string{"command"}),
		Permission:   "write",
		NeedsConfirm: true,
		Autonomous:   false,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			owner, provider, opts, unavail := boundShell(ctx, "builtin:shell.exec")
			if unavail != nil {
				return unavail, nil
			}
			cmd, _ := args["command"].(string)
			if cmd == "" {
				return map[string]any{"ok": false, "error": "missing command"}, nil
			}
			h, err := mgr.Ensure(ctx, owner, provider, opts)
			if err != nil {
				return map[string]any{"ok": false, "error": err.Error()}, nil
			}
			res, err := mgr.Exec(ctx, owner, h.ID, cmd)
			if err != nil {
				return map[string]any{"ok": false, "error": err.Error()}, nil
			}
			return map[string]any{
				"ok": true, "tool": "builtin:shell.exec", "handle_id": h.ID,
				"stdout": res.Stdout, "stderr": res.Stderr,
				"exit_code": res.ExitCode, "duration_ms": res.Duration,
			}, nil
		},
	})

	r.Register(ToolSpec{
		Name:        "builtin:shell.write",
		Description: "Write a file in this agent's shell (spawned lazily from the bound shell profile on first use and reused after).",
		Parameters: objectSchema(map[string]any{
			"path":    map[string]any{"type": "string", "description": "Path inside the shell"},
			"content": map[string]any{"type": "string", "description": "File content"},
		}, []string{"path", "content"}),
		Permission:   "write",
		NeedsConfirm: true,
		Autonomous:   false,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			owner, provider, opts, unavail := boundShell(ctx, "builtin:shell.write")
			if unavail != nil {
				return unavail, nil
			}
			path, _ := args["path"].(string)
			content, _ := args["content"].(string)
			if path == "" {
				return map[string]any{"ok": false, "error": "missing path"}, nil
			}
			h, err := mgr.Ensure(ctx, owner, provider, opts)
			if err != nil {
				return map[string]any{"ok": false, "error": err.Error()}, nil
			}
			if err := mgr.Write(ctx, owner, h.ID, path, []byte(content)); err != nil {
				return map[string]any{"ok": false, "error": err.Error()}, nil
			}
			return map[string]any{"ok": true, "path": path, "handle_id": h.ID}, nil
		},
	})

	r.Register(ToolSpec{
		Name:         "builtin:shell.destroy",
		Description:  "Tear down this agent's shell. A later shell call spawns a fresh one from the bound profile.",
		Parameters:   objectSchema(map[string]any{}, nil),
		Permission:   "write",
		NeedsConfirm: true,
		Autonomous:   false,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			owner, _, _, unavail := boundShell(ctx, "builtin:shell.destroy")
			if unavail != nil {
				return unavail, nil
			}
			h := mgr.Current(owner)
			if h == nil {
				return map[string]any{"ok": true, "destroyed": false}, nil
			}
			if err := mgr.Destroy(ctx, owner, h.ID); err != nil {
				return map[string]any{"ok": false, "error": err.Error()}, nil
			}
			return map[string]any{"ok": true, "destroyed": true, "handle_id": h.ID}, nil
		},
	})
}

// boundShell resolves the calling session's bound shell profile from the op
// context. The second-to-last return value is the honest-unavailable result —
// non-nil exactly when the agent has no shell profile bound (or the context
// carries no owner), in which case the caller returns it without touching the
// manager.
func boundShell(ctx context.Context, tool string) (owner, provider string, opts shell.SpawnOpts, unavailable map[string]any) {
	owner = session.OwnerFromCtx(ctx)
	profile := session.ShellFromCtx(ctx)
	if owner == "" || profile == nil {
		return "", "", shell.SpawnOpts{}, ResultUnavailable(tool, "This agent has no shell profile bound (agents.<name>.shell).")
	}
	provider, opts = spawnOptsFromProfile(profile)
	return owner, provider, opts, nil
}

// spawnOptsFromProfile converts a bound shell profile (the map shape built by
// shellProfileMap in main.go) into a provider name and SpawnOpts.
func spawnOptsFromProfile(profile map[string]any) (string, shell.SpawnOpts) {
	str := func(key string) string {
		s, _ := profile[key].(string)
		return s
	}
	provider := str("provider")
	if provider == "" {
		provider = "docker"
	}
	opts := shell.SpawnOpts{
		Image:    str("image"),
		WorkDir:  str("workdir"),
		Network:  str("network"),
		MemLimit: str("mem_limit"),
		Host:     str("host"),
		User:     str("user"),
		Password: str("password"),
		KeyFile:  str("key_file"),
	}
	if f, ok := profile["cpu_limit"].(float64); ok {
		opts.CPULimit = f
	}
	if env, ok := profile["env"].(map[string]string); ok {
		opts.Env = env
	}
	return provider, opts
}

func registerFSBuiltins(r *Registry, mgr *shell.Manager) {
	r.Register(ToolSpec{
		Name:        "builtin:fs.read",
		Description: "Read a file inside a shell handle.",
		Parameters: objectSchema(map[string]any{
			"handle_id": map[string]any{"type": "string", "description": "Shell handle ID"},
			"path":      map[string]any{"type": "string", "description": "Path inside the shell"},
		}, []string{"handle_id", "path"}),
		Permission: "read",
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			owner := session.OwnerFromCtx(ctx)
			handleID, _ := args["handle_id"].(string)
			path, _ := args["path"].(string)
			b, err := mgr.Read(ctx, owner, handleID, path)
			if err != nil {
				return map[string]any{"ok": false, "error": err.Error()}, nil
			}
			return map[string]any{"ok": true, "content": string(b), "path": path}, nil
		},
	})

	r.Register(ToolSpec{
		Name:        "builtin:fs.write",
		Description: "Write a file inside a shell handle.",
		Parameters: objectSchema(map[string]any{
			"handle_id": map[string]any{"type": "string", "description": "Shell handle ID"},
			"path":      map[string]any{"type": "string", "description": "Path inside the shell"},
			"content":   map[string]any{"type": "string", "description": "File content"},
		}, []string{"handle_id", "path", "content"}),
		Permission:   "write",
		NeedsConfirm: true,
		Autonomous:   false,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			owner := session.OwnerFromCtx(ctx)
			handleID, _ := args["handle_id"].(string)
			path, _ := args["path"].(string)
			content, _ := args["content"].(string)
			if err := mgr.Write(ctx, owner, handleID, path, []byte(content)); err != nil {
				return map[string]any{"ok": false, "error": err.Error()}, nil
			}
			return map[string]any{"ok": true, "path": path}, nil
		},
	})
}

func objectSchema(props map[string]any, required []string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}
