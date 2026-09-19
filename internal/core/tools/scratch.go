package tools

import (
	"bytes"
	"context"

	"agentflow/internal/core/files"
	"agentflow/internal/core/session"
)

// saveToScratch persists a tool's byte payload into the calling session's
// scratch space and records the outcome in the tool result. The scratch key
// is the session key stamped by the actor (falling back to the agent-name
// owner for direct/registry invokes that never passed through an actor); a
// nil manager or a failed save is reported in the result — "saved" with an
// error field — rather than failing the tool call, because the primary
// payload (the text result) is still useful.
func saveToScratch(ctx context.Context, fm *files.Manager, out map[string]any, name string, body []byte, mime string) {
	if fm == nil {
		out["saved"] = map[string]any{"name": name, "error": "files store unavailable (disabled at boot)"}
		return
	}
	owner := session.SessionKeyFromCtx(ctx)
	if owner == "" {
		owner = session.OwnerFromCtx(ctx)
	}
	e, err := fm.ScratchPut(ctx, owner, name, bytes.NewReader(body), mime)
	if err != nil {
		out["saved"] = map[string]any{"name": name, "error": err.Error()}
		return
	}
	saved := map[string]any{"name": e.Path, "handle": e.Handle, "size": e.Size}
	if t, _ := out["truncated"].(bool); t {
		saved["truncated"] = true
	}
	out["saved"] = saved
}
