package caps

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"agentflow/internal/core/files"
	"agentflow/internal/core/session"
)

// FileHandlers exposes the user-scoped file store as session op handlers,
// gated by the "files" capability at assembly time (see Gate). Scope is
// engine-resolved per call: "user:<uuid>" when the turn is channel-originated
// (the actor stamps the tenant UUID into ctx), otherwise "agent:<agent>". A
// loop can never name another scope. Bytes never cross the bridge: put takes
// content (or base64 data), read returns the handle + metadata. A nil manager
// (file store disabled at boot, e.g. unresolvable S3 credentials) makes every
// op fail honestly instead of panicking.
func FileHandlers(m *files.Manager, agent string) map[string]session.OpHandler {
	disabled := m == nil
	scope := func(ctx context.Context) string {
		if u := session.UserUUIDFromCtx(ctx); u != "" {
			return "user:" + u
		}
		return "agent:" + agent
	}
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
	guard := func() error {
		if disabled {
			return errors.New("files store unavailable (disabled at boot)")
		}
		return nil
	}
	// contentOf decodes the op's file content: Data (base64) wins over
	// Content (utf-8). One of the two must be present.
	contentOf := func(op session.Op) ([]byte, string, error) {
		if op.Data != "" {
			b, err := base64.StdEncoding.DecodeString(op.Data)
			if err != nil {
				return nil, "", err
			}
			mime := op.Mime
			if mime == "" {
				mime = "application/octet-stream"
			}
			return b, mime, nil
		}
		if op.Content == "" {
			return nil, "", errNoContent
		}
		return []byte(op.Content), op.Mime, nil
	}

	return map[string]session.OpHandler{
		"files.put": func(ctx context.Context, op session.Op) (string, bool) {
			if err := guard(); err != nil {
				return fail(err)
			}
			b, mime, err := contentOf(op)
			if err != nil {
				return fail(err)
			}
			e, err := m.Put(ctx, scope(ctx), op.Project, op.Path, bytes.NewReader(b), mime)
			if err != nil {
				return fail(err)
			}
			return okJSON(map[string]any{"ok": true, "entry": e})
		},

		// files.read returns the handle + metadata; raw bytes never cross the
		// Lua bridge (materialization is engine-side: shell mounts, llm parts).
		"files.read": func(ctx context.Context, op session.Op) (string, bool) {
			if err := guard(); err != nil {
				return fail(err)
			}
			e, err := m.Get(ctx, scope(ctx), op.Project, op.Path)
			if err != nil {
				return fail(err)
			}
			return okJSON(map[string]any{"ok": true, "entry": e})
		},

		"files.list": func(ctx context.Context, op session.Op) (string, bool) {
			if err := guard(); err != nil {
				return fail(err)
			}
			entries, err := m.List(ctx, scope(ctx), op.Project, op.Path)
			if err != nil {
				return fail(err)
			}
			return okJSON(map[string]any{"ok": true, "entries": entries})
		},

		"files.delete": func(ctx context.Context, op session.Op) (string, bool) {
			if err := guard(); err != nil {
				return fail(err)
			}
			if err := m.Delete(ctx, scope(ctx), op.Project, op.Path); err != nil {
				return fail(err)
			}
			return `{"ok":true}`, true
		},

		"files.commit": func(ctx context.Context, op session.Op) (string, bool) {
			if err := guard(); err != nil {
				return fail(err)
			}
			ref := op.Ref
			if ref == "" {
				ref = "main"
			}
			c, err := m.Commit(ctx, scope(ctx), op.Project, ref, op.CommitMsg)
			if err != nil {
				return fail(err)
			}
			return okJSON(map[string]any{"ok": true, "commit": c})
		},

		// files.checkout resolves a ref or commit id to a manifest
		// ({commit, files:[{path,handle,size}]}); consumers materialize
		// engine-side.
		"files.checkout": func(ctx context.Context, op session.Op) (string, bool) {
			if err := guard(); err != nil {
				return fail(err)
			}
			man, err := m.Checkout(ctx, scope(ctx), op.Project, op.Ref)
			if err != nil {
				return fail(err)
			}
			return okJSON(map[string]any{"ok": true, "manifest": man})
		},

		"files.scratch.put": func(ctx context.Context, op session.Op) (string, bool) {
			if err := guard(); err != nil {
				return fail(err)
			}
			b, mime, err := contentOf(op)
			if err != nil {
				return fail(err)
			}
			e, err := m.ScratchPut(ctx, op.Owner, op.Path, bytes.NewReader(b), mime)
			if err != nil {
				return fail(err)
			}
			return okJSON(map[string]any{"ok": true, "entry": e})
		},

		"files.scratch.read": func(ctx context.Context, op session.Op) (string, bool) {
			if err := guard(); err != nil {
				return fail(err)
			}
			e, err := m.ScratchGet(ctx, op.Owner, op.Path)
			if err != nil {
				return fail(err)
			}
			return okJSON(map[string]any{"ok": true, "entry": e})
		},

		"files.scratch.list": func(ctx context.Context, op session.Op) (string, bool) {
			if err := guard(); err != nil {
				return fail(err)
			}
			entries, err := m.ScratchList(ctx, op.Owner)
			if err != nil {
				return fail(err)
			}
			return okJSON(map[string]any{"ok": true, "entries": entries})
		},
	}
}

var errNoContent = errors.New("files.put requires content or data")
