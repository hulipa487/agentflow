// Runtime config surface: op handlers that serve deployment configuration to
// loops — the merged trigger list and gated credential access.
package caps

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"agentflow/internal/config"
	"agentflow/internal/core/credentials"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/session"
)

// RuntimeHandlers serves the merged trigger list (configdir triggers/*.yaml,
// or the top-level triggers: key) as read-only Lua data. The list is
// snapshotted at boot — a loop calling runtime.triggers() replaces the
// downstream pattern of baking CRON_TASKS / ROUTES tables into Lua source.
// Cron/every expressions pass through verbatim; the engine never reduces
// them to timers.
func RuntimeHandlers(triggers []config.Trigger) map[string]session.OpHandler {
	out := make([]map[string]any, 0, len(triggers))
	for _, tr := range triggers {
		entry := map[string]any{
			"name":        tr.Name,
			"run_on_boot": tr.RunOnBoot,
			"target":      map[string]any{"profile": tr.Target.Profile},
			"payload":     tr.Payload,
		}
		switch {
		case tr.Event != nil:
			entry["kind"] = "event"
			entry["event"] = map[string]any{"channel": tr.Event.Channel, "match": tr.Event.Match}
		case tr.Cron != "":
			entry["kind"] = "cron"
			entry["cron"] = tr.Cron
		case tr.Every != "":
			entry["kind"] = "every"
			entry["every"] = tr.Every
		default:
			entry["kind"] = "unknown"
		}
		out = append(out, entry)
	}
	b, err := json.Marshal(out)
	if err != nil {
		b = []byte("[]") // triggers carry no unmarshalable values
	}
	payload := string(b)
	return map[string]session.OpHandler{
		"runtime.triggers": func(ctx context.Context, op session.Op) (string, bool) {
			return `{"ok":true,"triggers":` + payload + `}`, true
		},
	}
}

// CredentialHandler answers credential.get: the stored secret is returned
// ONLY when the agent's profile allow-lists it (empty/missing list = denied),
// every access is logged and counted, and the value never reaches the
// message journal (op responses are not journaled; handlers never log it).
// store may be nil — the op then fails with a clear "not enabled" error.
func CredentialHandler(store *credentials.Store, allowed []string, log *slog.Logger) session.OpHandler {
	allow := map[string]bool{}
	for _, name := range allowed {
		allow[name] = true
	}
	fail := func(err error) (string, bool) {
		b, _ := json.Marshal(err.Error())
		return string(b), false
	}
	deny := func(ctx context.Context, owner, name, reason string) (string, bool) {
		metrics.Inc("agentflow_credential_gets_denied")
		log.Info("credential access denied", "agent", owner, "credential", name, "reason", reason)
		return fail(fmt.Errorf("credential.get %q denied: %s", name, reason))
	}
	return func(ctx context.Context, op session.Op) (string, bool) {
		owner := session.OwnerFromCtx(ctx)
		name := op.CredName
		if name == "" {
			return deny(ctx, owner, name, "name is required")
		}
		if store == nil {
			return deny(ctx, owner, name, "credentials are not enabled on this runtime")
		}
		if !allow[name] {
			reason := "the agent's credentials allow-list is empty or missing"
			if len(allow) > 0 {
				reason = "not in the agent's credentials allow-list"
			}
			return deny(ctx, owner, name, reason)
		}
		sec, ok, err := store.Get(ctx, "", name) // engine-wide tenancy
		if err != nil {
			metrics.Inc("agentflow_credential_gets_denied")
			log.Info("credential access failed", "agent", owner, "credential", name)
			return fail(fmt.Errorf("credential.get %q: %w", name, err))
		}
		if !ok {
			metrics.Inc("agentflow_credential_gets_denied")
			log.Info("credential access denied", "agent", owner, "credential", name, "reason", "no such credential")
			return fail(fmt.Errorf("credential.get %q: no such credential", name))
		}
		metrics.Inc("agentflow_credential_gets")
		// The log line names the access, never the value.
		log.Info("credential access granted", "agent", owner, "credential", name)
		b, _ := json.Marshal(map[string]any{"ok": true, "name": name, "value": sec.Value})
		return string(b), true
	}
}
