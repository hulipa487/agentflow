// The agent admin API: runtime agent upsert and removal (E20). The control
// plane (F22) owns tenant fleet definitions and calls these; AgentFlow only
// applies them. Applied changes are runtime-local by design — like models,
// they live until restart unless the caller also updates its own fleet store.
package webui

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"

	"agentflow/internal/config"
	"agentflow/internal/core/supervisor"
)

// agentNameRe keeps API-created agent names path-, YAML- and session-key
// safe. Config-file names may be broader; the registry is the stricter
// surface because its names are written by machines.
var agentNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// agentControl returns the registry and builder, or reports a named reason.
// Every route here dereferences both, and both are absent in a console wired
// for state only (or a test) — so absence is a 503, never a panic.
func (u *UI) agentControl(w http.ResponseWriter) (*supervisor.Supervisor, func(string, config.Agent) (*supervisor.AgentDef, error), bool) {
	if u.deps.Agents == nil || u.deps.BuildAgent == nil {
		writeErr(w, http.StatusServiceUnavailable, "the agent registry is not wired on this instance")
		return nil, nil, false
	}
	return u.deps.Agents, u.deps.BuildAgent, true
}

func (u *UI) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	sup, _, ok := u.agentControl(w)
	if !ok {
		return
	}
	views := []map[string]any{}
	for name, def := range sup.Agents() {
		if def.SpawnTemplate != nil {
			continue // spawn profiles are templates, not agents
		}
		caps := []string{}
		for c := range def.Capabilities {
			if def.Capabilities[c] {
				caps = append(caps, c)
			}
		}
		sort.Strings(caps)
		views = append(views, map[string]any{
			"name":         name,
			"model":        def.Info.Model,
			"persistent":   def.Persistent,
			"capabilities": caps,
			"loop_file":    def.LoopFile,
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i]["name"].(string) < views[j]["name"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "agents": views})
}

// handleAgentUpsert installs or replaces an agent at runtime. The body is the
// same YAML shape the config file's agents: entry takes — one format, because
// a fleet definition the control plane stores should read identically whether
// it boots the engine or edits it live. Strict decoding: a typo'd key is a
// 400, not a silently ignored field.
func (u *UI) handleAgentUpsert(w http.ResponseWriter, r *http.Request) {
	sup, build, ok := u.agentControl(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !agentNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "agent name must be 1-64 chars of letters, digits, _ or -")
		return
	}
	if cfg := u.deps.Cfg; cfg != nil {
		if _, exists := cfg.Agents[name]; !exists {
			for _, prefix := range []string{"__spawn__", "spawn:"} {
				if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
					writeErr(w, http.StatusBadRequest, "agent name is in the spawn-profile namespace")
					return
				}
			}
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read body")
		return
	}
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	var a config.Agent
	if err := dec.Decode(&a); err != nil {
		writeErr(w, http.StatusBadRequest, "bad agent definition: "+err.Error())
		return
	}
	if a.Loop == "" {
		writeErr(w, http.StatusBadRequest, "agent definition has no loop")
		return
	}
	if cfg := u.deps.Cfg; cfg != nil {
		if err := cfg.ValidateAgent(name, a, "admin api"); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	def, err := build(name, a)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	_, replaced := sup.Agents()[name]
	if err := sup.UpsertAgent(name, def); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	note := "live on this instance; new sessions resolve it now"
	if replaced {
		note = "live on this instance; live sessions apply it at their next safe point"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"applied":  "runtime",
		"replaced": replaced,
		"note":     note + "; it is not persisted and does not reach other instances until they upsert it too",
	})
}

func (u *UI) handleAgentRemove(w http.ResponseWriter, r *http.Request) {
	sup, _, ok := u.agentControl(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	force := r.URL.Query().Get("force") == "true"
	stopped, err := sup.RemoveAgent(name, force)
	var live *supervisor.LiveSessionsError
	switch {
	case err == nil:
	case errors.As(err, &live):
		writeErr(w, http.StatusConflict, live.Error())
		return
	default:
		var unknown *supervisor.UnknownAgentError
		if errors.As(err, &unknown) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"applied": "runtime",
		"stopped": stopped,
		"note":    "runtime-local; other instances remove it separately, and the definition returns at their restart if their config still has it",
	})
}
