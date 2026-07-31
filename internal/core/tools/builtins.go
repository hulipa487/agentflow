// Package tools registers the built-in tools that ship with the runtime.
package tools

import (
	"context"
	"strings"

	"agentflow/internal/core/session"
	"agentflow/internal/drivers/shell"
)

// RegisterBuiltins adds the phase-2 builtin tools to the registry.
func RegisterBuiltins(r *Registry) {
	r.Register(ToolSpec{
		Name:        "builtin:web_search",
		Description: "Search the public web. Returns honest unavailable if no search backend is configured.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query"},
			},
			"required": []string{"query"},
		},
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			return ResultUnavailable("builtin:web_search", "No search backend is configured."), nil
		},
	})
}

// RegisterShellBuiltins adds tools that operate inside shell handles.
func RegisterShellBuiltins(r *Registry, mgr *shell.Manager) {
	registerFSBuiltins(r, mgr)
	registerGitBuiltins(r, mgr)
}

func registerFSBuiltins(r *Registry, mgr *shell.Manager) {
	r.Register(ToolSpec{
		Name:        "builtin:fs.read",
		Description: "Read a file inside a shell handle.",
		Parameters: objectSchema(map[string]any{
			"handle_id": map[string]any{"type": "string", "description": "Shell handle ID"},
			"path":      map[string]any{"type": "string", "description": "Path inside the shell"},
		}, []string{"handle_id", "path"}),
		Permission:  "read",
		Autonomous:  true,
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

func registerGitBuiltins(r *Registry, mgr *shell.Manager) {
	gitRead := []struct {
		name string
		desc string
		cmd  string
	}{
		{"builtin:git.status", "Run git status inside a shell handle.", "git status --short"},
		{"builtin:git.diff", "Run git diff inside a shell handle.", "git diff --"},
		{"builtin:git.log", "Run git log inside a shell handle.", "git log --oneline -20"},
	}
	for _, gt := range gitRead {
		gt := gt
		r.Register(ToolSpec{
			Name:        gt.name,
			Description: gt.desc,
			Parameters: objectSchema(map[string]any{
				"handle_id": map[string]any{"type": "string", "description": "Shell handle ID"},
				"args":      map[string]any{"type": "string", "description": "Optional extra git args"},
			}, []string{"handle_id"}),
			Permission: "read",
			Autonomous: true,
			Invoke: func(ctx context.Context, args map[string]any) (any, error) {
				return runGit(ctx, mgr, args, gt.cmd, false)
			},
		})
	}

	gitWrite := []struct {
		name string
		desc string
		cmd  string
	}{
		{"builtin:git.commit", "Create a git commit inside a shell handle.", "git commit"},
		{"builtin:git.push", "Push commits inside a shell handle.", "git push"},
	}
	for _, gt := range gitWrite {
		gt := gt
		r.Register(ToolSpec{
			Name:        gt.name,
			Description: gt.desc,
			Parameters: objectSchema(map[string]any{
				"handle_id": map[string]any{"type": "string", "description": "Shell handle ID"},
				"args":      map[string]any{"type": "string", "description": "Extra git args"},
			}, []string{"handle_id"}),
			Permission:   "write",
			NeedsConfirm: true,
			Autonomous:   false,
			Invoke: func(ctx context.Context, args map[string]any) (any, error) {
				return runGit(ctx, mgr, args, gt.cmd, true)
			},
		})
	}
}

func runGit(ctx context.Context, mgr *shell.Manager, args map[string]any, base string, write bool) (any, error) {
	owner := session.OwnerFromCtx(ctx)
	handleID, _ := args["handle_id"].(string)
	extra, _ := args["args"].(string)
	cmd := base
	if extra != "" {
		cmd += " " + extra
	}
	if write && strings.Contains(cmd, "--no-verify") {
		return map[string]any{"ok": false, "error": "--no-verify is not allowed"}, nil
	}
	res, err := mgr.Exec(ctx, owner, handleID, cmd)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}, nil
	}
	return map[string]any{"ok": res.ExitCode == 0, "stdout": res.Stdout, "stderr": res.Stderr, "exit_code": res.ExitCode}, nil
}

func objectSchema(props map[string]any, required []string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}
