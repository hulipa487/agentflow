// The Studio admin API: agent builder (read/validate/apply/persist), the
// playground chat stream, and the trace ring. It is mounted with the same
// bearer-token check as the rest of the console and is inert without it.
//
// The engine-side state (trace rings, chat correlation, workspace writes)
// lives in internal/core/studio; this file is the HTTP shape.
package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"agentflow/internal/config"
	"agentflow/internal/core/studio"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/vm"
)

// studioReady returns the handles every route needs, or answers 503 with a
// named reason.
func (u *UI) studioReady(w http.ResponseWriter) (*studio.Broker, *studio.Workspace, bool) {
	if u.deps.Studio == nil || u.deps.Workspace == nil {
		writeErr(w, http.StatusServiceUnavailable, "the studio API is not enabled on this instance")
		return nil, nil, false
	}
	return u.deps.Studio, u.deps.Workspace, true
}

func (u *UI) handleStudioCatalog(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := u.studioReady(w); !ok {
		return
	}
	if u.deps.StudioCatalog == nil {
		writeErr(w, http.StatusServiceUnavailable, "the studio catalog is not wired on this instance")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "catalog": u.deps.StudioCatalog()})
}

// studioAgentRow is one agent in the builder list.
type studioAgentRow struct {
	Name         string   `json:"name"`
	Model        string   `json:"model,omitempty"`
	Loop         string   `json:"loop,omitempty"`
	Spawn        bool     `json:"spawn"`
	Persistent   bool     `json:"persistent"`
	Capabilities []string `json:"capabilities"`
	Persisted    bool     `json:"persisted"`
	Sessions     int      `json:"sessions"`
}

func (u *UI) handleStudioAgents(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := u.studioReady(w)
	if !ok {
		return
	}
	sup := u.deps.Agents
	if sup == nil {
		writeErr(w, http.StatusServiceUnavailable, "the agent registry is not wired on this instance")
		return
	}
	live := map[string]int{}
	if u.deps.Snapshot != nil {
		rows, _, _ := u.deps.Snapshot()
		for _, row := range rows {
			live[row.Agent]++
		}
	}
	out := []studioAgentRow{}
	for name, def := range sup.Agents() {
		row := studioAgentRow{
			Name:       name,
			Spawn:      def.SpawnTemplate != nil,
			Persistent: def.Persistent,
			Persisted:  ws.Persisted(name),
			Sessions:   live[name],
		}
		if def.Info != nil {
			row.Model = def.Info.Model
			row.Capabilities = capList(def.Capabilities)
		}
		row.Loop = def.LoopFile
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "agents": out})
}

func (u *UI) handleStudioAgentGet(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := u.studioReady(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	sup := u.deps.Agents
	var def *supervisor.AgentDef
	if sup != nil {
		def = sup.Agents()[name]
	}
	a, haveConfig := config.Agent{}, false
	source := "config"
	if applied, ok := u.studioAppliedAgent(name); ok {
		// A live apply is the newest definition; the boot config is a startup
		// snapshot and a persisted fragment may lag behind it.
		a, haveConfig, source = applied, true, "runtime"
	} else if u.deps.Cfg != nil {
		a, haveConfig = u.deps.Cfg.Agents[name]
	}
	persisted := ws.Persisted(name)

	loopSrc, instrSrc := "", ""
	files := ws.Files(name)
	if !haveConfig {
		switch {
		case persisted:
			pa, ploop, pinstr, pf, err := ws.Load(name)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			a, loopSrc, instrSrc, files = pa, ploop, pinstr, pf
			source = "persisted"
		case def != nil:
			// Runtime-only: no config entry, no fragment. Reconstruct what the
			// def retains; the loop and instructions below come from the live
			// session definition itself.
			a = agentFromDef(name, def)
			source = "runtime"
		default:
			writeErr(w, http.StatusNotFound, "no such agent")
			return
		}
	}

	// The live definition wins for the loop and instructions when it exists:
	// it is what the playground actually runs.
	if def != nil {
		if def.LoopFile != "" {
			if b, err := readFile(def.LoopFile); err == nil {
				loopSrc = b
			}
		} else if def.LoopSrc != "" {
			loopSrc = def.LoopSrc
		}
		if def.Info != nil && def.Info.Instructions != nil {
			instrSrc = def.Info.Instructions.Load()
		}
	}
	// A persisted fragment fills anything the live definition could not
	// provide (a runtime-only def that never carried an instructions box).
	if persisted && (loopSrc == "" || instrSrc == "") {
		if _, ploop, pinstr, _, err := ws.Load(name); err == nil {
			if loopSrc == "" {
				loopSrc = ploop
			}
			if instrSrc == "" {
				instrSrc = pinstr
			}
		}
	}
	yamlText, err := studio.MarshalAgent(a)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sessions := 0
	if u.deps.Snapshot != nil && def != nil {
		rows, _, _ := u.deps.Snapshot()
		for _, row := range rows {
			if row.Agent == name {
				sessions++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"name":         name,
		"source":       source,
		"agent_yaml":   yamlText,
		"agent":        agentJSON(a),
		"loop":         loopSrc,
		"instructions": instrSrc,
		"files":        files,
		"persisted":    persisted,
		"spawn":        def != nil && def.SpawnTemplate != nil,
		"persistent":   def != nil && def.Persistent,
		"model":        a.Model,
		"capabilities": capListOf(a.Capabilities),
		"skills":       a.Skills,
		"sessions":     sessions,
	})
}

// studioAgentJSON is the builder form's view of a definition. Only fields the
// form edits are flattened; anything else (inline memory stores, arbitrary
// extras) stays available through the raw YAML pane.
type studioAgentJSON struct {
	Loop          string         `json:"loop"`
	Model         string         `json:"model"`
	HistoryBudget int            `json:"history_budget"`
	Safety        string         `json:"safety"`
	Shell         string         `json:"shell,omitempty"`
	Skills        []string       `json:"skills"`
	Capabilities  []string       `json:"capabilities"`
	CanContact    []string       `json:"can_contact"`
	Persistent    bool           `json:"persistent"`
	Credentials   []string       `json:"credentials"`
	InheritUser   *bool          `json:"inherit_user,omitempty"`
	Extras        map[string]any `json:"extras,omitempty"`
}

func agentJSON(a config.Agent) studioAgentJSON {
	return studioAgentJSON{
		Loop:          a.Loop,
		Model:         a.Model,
		HistoryBudget: a.HistoryBudget,
		Safety:        a.Safety,
		Shell:         a.Shell,
		Skills:        append([]string(nil), a.Skills...),
		Capabilities:  append([]string(nil), a.Capabilities...),
		CanContact:    append([]string(nil), a.CanContact...),
		Persistent:    a.Persistent,
		Credentials:   append([]string(nil), a.Credentials...),
		InheritUser:   a.InheritUser,
		Extras:        a.Extras,
	}
}

// agentFromDef reconstructs an edit-able definition for an agent that exists
// only in the runtime registry (applied but never persisted). Fields the def
// does not retain (safety name, memory profile) are absent rather than
// guessed; the next persist writes what the form shows.
func agentFromDef(name string, def *supervisor.AgentDef) config.Agent {
	a := config.Agent{
		Loop:       def.LoopFile,
		Persistent: def.Persistent,
		Capabilities: func() []string {
			out := []string{}
			for c, on := range def.Capabilities {
				if on {
					out = append(out, c)
				}
			}
			sort.Strings(out)
			return out
		}(),
	}
	if def.Info != nil {
		a.Model = def.Info.Model
		a.Skills = append([]string(nil), def.Info.Skills...)
		a.Extras = def.Info.Extras
		a.Credentials = def.Info.Credentials
		a.InheritUser = def.Info.InheritUser
		if def.Info.InstructionsPath != "" {
			a.Instructions = config.InstructionsRef{File: def.Info.InstructionsPath}
		}
		if def.Info.InstructionsPrompt != "" {
			a.Instructions = config.InstructionsRef{Prompt: def.Info.InstructionsPrompt}
		}
	}
	return a
}

// studioAgentRequest is the builder's write body. agent_yaml is the same shape
// the config's agents: entry takes; loop and instructions are the sources the
// builder edits (they are written to the workspace before they are applied).
type studioAgentRequest struct {
	Name         string `json:"name"`
	AgentYAML    string `json:"agent_yaml"`
	Loop         string `json:"loop"`
	Instructions string `json:"instructions"`
	Apply        *bool  `json:"apply"`
	Persist      bool   `json:"persist"`
	Force        bool   `json:"force"`
	ExpectMtime  int64  `json:"expect_mtime"`
}

func decodeStudioAgent(text string) (config.Agent, error) {
	var a config.Agent
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(&a); err != nil {
		return a, fmt.Errorf("bad agent definition: %w", err)
	}
	if a.Loop == "" {
		return a, fmt.Errorf("agent definition has no loop")
	}
	return a, nil
}

func (u *UI) validateStudioAgent(name string, a config.Agent, loop string) []string {
	var errs []string
	if u.deps.Cfg != nil {
		if a.Model != "" {
			if _, ok := u.deps.Cfg.Models[a.Model]; !ok {
				errs = append(errs, fmt.Sprintf("agent %q references unknown model %q", name, a.Model))
			}
		}
		if err := u.deps.Cfg.ValidateAgent(name, a, "studio"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if strings.TrimSpace(loop) == "" {
		errs = append(errs, "loop source is empty")
	} else {
		if err := vm.CheckChunkVersion(name+".lua", loop); err != nil {
			errs = append(errs, err.Error())
		}
		if err := vm.CompileCheck(name+".lua", loop); err != nil {
			errs = append(errs, err.Error())
		}
	}
	return errs
}

func (u *UI) handleStudioValidate(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := u.studioReady(w); !ok {
		return
	}
	req, a, ok := u.readAgentRequest(w, r)
	if !ok {
		return
	}
	errs := u.validateStudioAgent(req.Name, a, req.Loop)
	warnings := []string{}
	if !strings.Contains(req.Loop, "function loop") {
		warnings = append(warnings, "the loop source does not define function loop()")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": len(errs) == 0, "errors": errs, "warnings": warnings})
}

func (u *UI) readAgentRequest(w http.ResponseWriter, r *http.Request) (studioAgentRequest, config.Agent, bool) {
	var req studioAgentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return req, config.Agent{}, false
	}
	if req.Name == "" {
		req.Name = r.PathValue("name")
	}
	req.Name = strings.TrimSpace(req.Name)
	a, err := decodeStudioAgent(req.AgentYAML)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "errors": []string{err.Error()}})
		return req, config.Agent{}, false
	}
	return req, a, true
}

func (u *UI) handleStudioAgentPut(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := u.studioReady(w)
	if !ok {
		return
	}
	sup := u.deps.Agents
	if sup == nil || u.deps.BuildAgent == nil {
		writeErr(w, http.StatusServiceUnavailable, "the agent registry is not wired on this instance")
		return
	}
	req, a, ok := u.readAgentRequest(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		name = req.Name
	}
	if name == "" {
		writeErr(w, http.StatusBadRequest, "agent name is required")
		return
	}
	if !agentNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "agent name must be 1-64 chars of letters, digits, _ or -")
		return
	}
	if errs := u.validateStudioAgent(name, a, req.Loop); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "errors": errs})
		return
	}

	apply := req.Apply == nil || *req.Apply
	applied := false
	if apply {
		loopPath, promptPath, err := ws.Stage(name, req.Loop, req.Instructions)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "errors": []string{err.Error()}})
			return
		}
		build := a
		build.Loop = loopPath
		if req.Instructions != "" {
			build.Instructions = config.InstructionsRef{File: promptPath}
		} else {
			build.Instructions = config.InstructionsRef{}
		}
		def, err := u.deps.BuildAgent(name, build)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "errors": []string{err.Error()}})
			return
		}
		if err := sup.UpsertAgent(name, def); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "errors": []string{err.Error()}})
			return
		}
		u.rememberStudioAgent(name, a)
		applied = true
	}

	resp := map[string]any{"ok": true, "applied": applied}
	if req.Persist {
		files, err := ws.Save(name, a, req.Loop, req.Instructions, req.ExpectMtime, req.Force)
		if err != nil {
			// The runtime is already on the new definition; a persist failure
			// is reported as a warning rather than undoing the apply.
			resp["persisted"] = false
			resp["persist_error"] = err.Error()
		} else {
			resp["persisted"] = true
			resp["files"] = files
		}
	}
	if applied {
		resp["note"] = "live on this instance; persisted definitions load at the next boot"
	} else {
		resp["note"] = "validated but not applied"
	}
	writeJSON(w, http.StatusOK, resp)
}

func (u *UI) handleStudioAgentDelete(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := u.studioReady(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	force := r.URL.Query().Get("force") == "true"
	persist := r.URL.Query().Get("persist") == "true"
	// Refuse the half-delete: with persist on, a hand-written profile is not
	// ours to remove, and the runtime agent must not disappear while its
	// definition stays in the config.
	if persist && !force && ws.Mode() == "configdir" && ws.Persisted(name) && !ws.Files(name).Managed {
		writeErr(w, http.StatusConflict, "this profile was not created by Studio; pass force=true to delete it")
		return
	}
	stopped := 0
	removedRuntime := false
	if u.deps.Agents != nil {
		n, err := u.deps.Agents.RemoveAgent(name, force)
		var live *supervisor.LiveSessionsError
		var unknown *supervisor.UnknownAgentError
		switch {
		case err == nil:
			stopped, removedRuntime = n, true
		case errors.As(err, &live):
			writeErr(w, http.StatusConflict, live.Error())
			return
		case errors.As(err, &unknown):
			// A persisted-only agent has no runtime definition to remove.
		default:
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	resp := map[string]any{"ok": true, "removed_runtime": removedRuntime, "stopped": stopped}
	if persist {
		removed, err := ws.Remove(name, force)
		if err != nil {
			if !removedRuntime {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			resp["persist_error"] = err.Error()
		} else {
			resp["persisted"] = true
			resp["removed_files"] = removed
		}
	}
	u.forgetStudioAgent(name)
	writeJSON(w, http.StatusOK, resp)
}

// ---- playground ---------------------------------------------------------

type studioChatRequest struct {
	Agent     string `json:"agent"`
	Thread    string `json:"thread"`
	Text      string `json:"text"`
	ActAs     string `json:"act_as_user"`
	TimeoutMS int    `json:"timeout_ms"`
	Stream    *bool  `json:"stream"`
}

func (u *UI) handleStudioChat(w http.ResponseWriter, r *http.Request) {
	broker, _, ok := u.studioReady(w)
	if !ok {
		return
	}
	var req studioChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Agent == "" || strings.TrimSpace(req.Text) == "" {
		writeErr(w, http.StatusBadRequest, "agent and text are required")
		return
	}
	if def := agentDef(u.deps.Agents, req.Agent); def == nil {
		writeErr(w, http.StatusNotFound, "unknown agent "+req.Agent)
		return
	} else if def.SpawnTemplate != nil {
		writeErr(w, http.StatusBadRequest, "spawn profiles are templates; chat with a configured agent")
		return
	}
	chatReq := studio.ChatRequest{Agent: req.Agent, Thread: req.Thread, Text: req.Text, ActAs: req.ActAs}
	if req.TimeoutMS > 0 {
		chatReq.Timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}

	stream := req.Stream == nil || *req.Stream
	if !stream {
		var events []studio.ChatEvent
		res, err := broker.Chat(r.Context(), chatReq, func(ev studio.ChatEvent) { events = append(events, ev) })
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res, "events": events})
		return
	}

	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	_, _ = broker.Chat(r.Context(), chatReq, func(ev studio.ChatEvent) {
		writeSSE(w, fl, ev.Type, ev)
	})
}

func (u *UI) handleStudioThreads(w http.ResponseWriter, r *http.Request) {
	broker, _, ok := u.studioReady(w)
	if !ok {
		return
	}
	agent := r.URL.Query().Get("agent")
	threads := broker.Threads()
	if agent != "" {
		filtered := threads[:0]
		for _, t := range threads {
			if t.Agent == agent {
				filtered = append(filtered, t)
			}
		}
		threads = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "threads": threads})
}

func (u *UI) handleStudioStopThread(w http.ResponseWriter, r *http.Request) {
	broker, _, ok := u.studioReady(w)
	if !ok {
		return
	}
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent is required")
		return
	}
	broker.StopThread(agent, r.PathValue("thread"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (u *UI) handleStudioTrace(w http.ResponseWriter, r *http.Request) {
	broker, _, ok := u.studioReady(w)
	if !ok {
		return
	}
	agent := r.PathValue("agent")
	thread := r.PathValue("thread")
	if r.URL.Query().Get("stream") == "true" {
		u.streamTrace(w, r, broker, agent, thread)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "events": broker.TraceSnapshot(agent, thread)})
}

func (u *UI) streamTrace(w http.ResponseWriter, r *http.Request, broker *studio.Broker, agent, thread string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	ch, unsub := broker.SubscribeSession(agent + "|" + studio.ChannelName + ":" + thread)
	defer unsub()
	keep := time.NewTicker(15 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, fl, "trace", ev)
		case <-keep.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}

// ---- helpers ------------------------------------------------------------

func (u *UI) rememberStudioAgent(name string, a config.Agent) {
	u.mu.Lock()
	if u.studioApplied == nil {
		u.studioApplied = map[string]config.Agent{}
	}
	u.studioApplied[name] = a
	u.mu.Unlock()
}

func (u *UI) studioAppliedAgent(name string) (config.Agent, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	a, ok := u.studioApplied[name]
	return a, ok
}

func (u *UI) forgetStudioAgent(name string) {
	u.mu.Lock()
	delete(u.studioApplied, name)
	u.mu.Unlock()
}

func agentDef(sup *supervisor.Supervisor, name string) *supervisor.AgentDef {
	if sup == nil {
		return nil
	}
	return sup.Agents()[name]
}

func capList(m map[string]bool) []string {
	out := []string{}
	for c, on := range m {
		if on {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func capListOf(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func writeSSE(w io.Writer, fl http.Flusher, event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	fl.Flush()
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
