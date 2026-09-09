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
