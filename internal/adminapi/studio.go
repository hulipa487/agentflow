// The Studio admin API: agent builder (read/validate/apply/persist), the
// playground chat stream, and the trace ring. It is mounted with the same
// bearer-token check as the rest of the console and is inert without it.
//
// The engine-side state (trace rings, chat correlation, workspace writes)
// lives in internal/core/studio; this file is the HTTP shape.
package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"agentflow/internal/config"
	"agentflow/internal/core/execguard"
	"agentflow/internal/core/media"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
	"agentflow/internal/core/studio"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/looptest"
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

type loadedAgentDef struct {
	agent     config.Agent
	loopSrc   string
	instrSrc  string
	files     studio.AgentFiles
	source    string
	persisted bool
	def       *supervisor.AgentDef
}

func (u *UI) loadAgentDefinition(ws *studio.Workspace, name string) (loadedAgentDef, int, error) {
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
				return loadedAgentDef{}, http.StatusInternalServerError, err
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
			return loadedAgentDef{}, http.StatusNotFound, errors.New("no such agent")
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
	return loadedAgentDef{
		agent:     a,
		loopSrc:   loopSrc,
		instrSrc:  instrSrc,
		files:     files,
		source:    source,
		persisted: persisted,
		def:       def,
	}, http.StatusOK, nil
}

func (u *UI) handleStudioAgentGet(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := u.studioReady(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	loaded, status, err := u.loadAgentDefinition(ws, name)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	a, def := loaded.agent, loaded.def
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
		"source":       loaded.source,
		"agent_yaml":   yamlText,
		"agent":        agentJSON(a),
		"loop":         loaded.loopSrc,
		"instructions": loaded.instrSrc,
		"files":        loaded.files,
		"persisted":    loaded.persisted,
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
	Loop          string `json:"loop"`
	Model         string `json:"model"`
	HistoryBudget int    `json:"history_budget"`
	Safety        string `json:"safety"`
	Shell         string `json:"shell,omitempty"`
	SSH           string `json:"ssh,omitempty"`
	// Memory is the memory profile the agent names ("conversational" is the
	// built-in one). MemoryInline is an inline profile, as the file spells it,
	// which the builder keeps as it is and the raw YAML pane edits.
	Memory             string               `json:"memory,omitempty"`
	MemoryInline       map[string]any       `json:"memory_inline,omitempty"`
	Skills             []string             `json:"skills"`
	Capabilities       []string             `json:"capabilities"`
	CanContact         []string             `json:"can_contact"`
	Persistent         bool                 `json:"persistent"`
	Credentials        []string             `json:"credentials"`
	InheritUser        *bool                `json:"inherit_user,omitempty"`
	InstructionsPrompt string               `json:"instructions_prompt,omitempty"`
	Budget             *config.BudgetConfig `json:"budget,omitempty"`
	Extras             map[string]any       `json:"extras,omitempty"`
}

func agentJSON(a config.Agent) studioAgentJSON {
	var budget *config.BudgetConfig
	if a.Budget.TokensPerDay > 0 || a.Budget.Window != "" {
		b := a.Budget
		budget = &b
	}
	return studioAgentJSON{
		Loop:               a.Loop,
		Model:              a.Model,
		HistoryBudget:      a.HistoryBudget,
		Safety:             a.Safety,
		Shell:              a.Shell,
		SSH:                a.SSH,
		Memory:             a.Memory.Profile,
		MemoryInline:       inlineMemory(a.Memory),
		Skills:             append([]string(nil), a.Skills...),
		Capabilities:       append([]string(nil), a.Capabilities...),
		CanContact:         append([]string(nil), a.CanContact...),
		Persistent:         a.Persistent,
		Credentials:        append([]string(nil), a.Credentials...),
		InheritUser:        a.InheritUser,
		InstructionsPrompt: a.Instructions.Prompt,
		Budget:             budget,
		Extras:             a.Extras,
	}
}

// inlineMemory renders an inline memory profile as the file spells it, so
// the builder can write it back unchanged.
func inlineMemory(m config.MemoryAgentConfig) map[string]any {
	if !m.IsInline {
		return nil
	}
	return studio.MemoryProfileMap(m.Inline)
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

func (u *UI) currentConfigForValidation() *config.Config {
	// Prefer a fresh disk read so newly saved safety/memory/shell/prompt
	// profiles are visible to Studio validation immediately without restarting,
	// then overlay live models and live agents from the running registries.
	var base *config.Config
	if u.deps.ConfigDir != "" {
		if loaded, err := config.LoadDir(u.deps.ConfigDir, nil); err == nil && loaded != nil {
			base = loaded
		}
	} else if u.deps.ConfigPath != "" {
		if loaded, err := config.Load(u.deps.ConfigPath); err == nil && loaded != nil {
			base = loaded
		}
	}
	if base == nil {
		if u.deps.Cfg == nil {
			return nil
		}
		cp := *u.deps.Cfg
		base = &cp
	}
	// Overlay live agents so can_contact recognizes agents installed via Studio.
	if u.deps.Agents != nil {
		mergedAgents := make(map[string]config.Agent, len(base.Agents))
		for k, v := range base.Agents {
			mergedAgents[k] = v
		}
		for name, def := range u.deps.Agents.Agents() {
			if def == nil || def.SpawnTemplate != nil {
				continue
			}
			if _, exists := mergedAgents[name]; !exists {
				mergedAgents[name] = agentFromDef(name, def)
			}
		}
		base.Agents = mergedAgents
	}
	return base
}

func (u *UI) validateStudioAgent(name string, a config.Agent, loop string) []string {
	var errs []string
	if a.Model != "" {
		found := false
		if u.deps.Models != nil {
			_, found = u.deps.Models.List()[a.Model]
		} else if u.deps.Cfg != nil {
			_, found = u.deps.Cfg.Models[a.Model]
		}
		if !found {
			errs = append(errs, fmt.Sprintf("agent %q references unknown model %q", name, a.Model))
		}
	}
	if cfg := u.currentConfigForValidation(); cfg != nil {
		if err := cfg.ValidateAgent(name, a, "studio"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if strings.TrimSpace(loop) == "" {
		errs = append(errs, "loop source is empty")
	} else {
		if err := vm.CheckChunkVersion(name+".ts", loop); err != nil {
			errs = append(errs, err.Error())
		}
		if err := vm.CompileCheck(name+".ts", loop); err != nil {
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
	if !strings.Contains(req.Loop, "function loop") && !strings.Contains(req.Loop, "loop =") {
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
		var loopPath, promptPath string
		var err error
		if req.Persist {
			loopPath, promptPath, err = ws.Stage(name, req.Loop, req.Instructions)
		} else {
			// Live-only Apply must not overwrite or delete hand-edited loops
			// or prompts in the config directory.
			loopPath, promptPath, err = ws.StageTransient(name, req.Loop, req.Instructions)
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "errors": []string{err.Error()}})
			return
		}
		build := a
		build.Loop = loopPath
		if a.Instructions.Prompt == "" {
			if req.Instructions != "" {
				build.Instructions = config.InstructionsRef{File: promptPath}
			} else {
				build.Instructions = config.InstructionsRef{}
			}
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
	Agent       string       `json:"agent"`
	Thread      string       `json:"thread"`
	Text        string       `json:"text"`
	Attachments []media.Part `json:"attachments,omitempty"`
	ActAs       string       `json:"act_as_user"`
	TimeoutMS   int          `json:"timeout_ms"`
	Stream      *bool        `json:"stream"`
}

func (u *UI) handleStudioChat(w http.ResponseWriter, r *http.Request) {
	broker, _, ok := u.studioReady(w)
	if !ok {
		return
	}
	var req studioChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 24<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Agent == "" || (strings.TrimSpace(req.Text) == "" && len(req.Attachments) == 0) {
		writeErr(w, http.StatusBadRequest, "agent and text (or attachments) are required")
		return
	}
	if def := agentDef(u.deps.Agents, req.Agent); def == nil {
		writeErr(w, http.StatusNotFound, "unknown agent "+req.Agent)
		return
	} else if def.SpawnTemplate != nil {
		writeErr(w, http.StatusBadRequest, "spawn profiles are templates; chat with a configured agent")
		return
	}
	attachments := req.Attachments
	if len(attachments) > 0 && u.deps.Media != nil {
		scope := "agent:" + req.Agent
		if actAs := strings.TrimSpace(req.ActAs); actAs != "" {
			scope = "user:" + actAs
		}
		norm, err := u.deps.Media.NormalizeBorder(r.Context(), scope, attachments)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("attachments: %v", err))
			return
		}
		attachments = norm
	}
	chatReq := studio.ChatRequest{
		Agent:       req.Agent,
		Thread:      req.Thread,
		Text:        req.Text,
		Attachments: attachments,
		ActAs:       req.ActAs,
	}
	if req.TimeoutMS > 0 {
		chatReq.Timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	// Refusals that need no turn (an act-as id that is not a profile) are
	// answered here, while a status code can still say so. Once the stream's
	// 200 is written, the only channel left is the stream itself.
	if err := broker.Check(chatReq); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	stream := req.Stream == nil || *req.Stream
	if !stream {
		var events []studio.ChatEvent
		res, err := broker.Chat(r.Context(), chatReq, func(ev studio.ChatEvent) { events = append(events, ev) })
		var loopErr *studio.LoopError
		if errors.As(err, &loopErr) {
			// The request was fine and the turn ran; it is the loop that
			// failed. The trace of how far it got is the useful part.
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "result": res, "events": events})
			return
		}
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

	said := false
	_, err := broker.Chat(r.Context(), chatReq, func(ev studio.ChatEvent) {
		if ev.Type == "error" {
			said = true
		}
		writeSSE(w, fl, ev.Type, ev)
	})
	// Every failure must reach the client as an event: a stream that simply
	// ends is indistinguishable from a dropped connection. A caller that has
	// gone away is the one failure with nobody left to tell.
	if err != nil && !said && r.Context().Err() == nil {
		writeSSE(w, fl, "error", studio.ChatEvent{Type: "error", Err: err.Error()})
	}
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

// handleStudioMedia streams a media handle to the authenticated Studio UI
// (for inline playground image previews and attachment downloads).
func (u *UI) handleStudioMedia(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := u.studioReady(w); !ok {
		return
	}
	if u.deps.Media == nil {
		writeErr(w, http.StatusServiceUnavailable, "media service is not enabled on this instance")
		return
	}
	handle := r.PathValue("handle")
	if !media.ValidHandle(handle) {
		writeErr(w, http.StatusBadRequest, "invalid media handle")
		return
	}
	blob, err := u.deps.Media.MaterializeBytes(r.Context(), media.ScopeMaintenance, media.Part{
		Handle: handle,
	}, media.TargetSpec{})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "no such file") {
			writeErr(w, http.StatusNotFound, "media handle not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ct := blob.MIME
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob.Bytes)
}

type studioLoopTestRequest struct {
	YAML         string `json:"yaml"`
	Agent        string `json:"agent,omitempty"`
	LoopSource   string `json:"loop_source,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Model        string `json:"model,omitempty"`
}

func (u *UI) handleStudioLoopTest(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := u.studioReady(w)
	if !ok {
		return
	}
	var req studioLoopTestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.YAML) == "" {
		writeErr(w, http.StatusBadRequest, "yaml is required")
		return
	}
	sc, err := looptest.Load([]byte(req.YAML))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid scenario YAML: %v", err))
		return
	}
	if agent := strings.TrimSpace(req.Agent); agent != "" {
		sc.Agent = agent
	}
	if strings.TrimSpace(req.LoopSource) != "" {
		sc.LoopSource = req.LoopSource
	}
	if strings.TrimSpace(req.Instructions) != "" {
		sc.Instructions = req.Instructions
	}
	if strings.TrimSpace(req.Model) != "" {
		sc.Model = req.Model
	}
	if strings.TrimSpace(sc.LoopSource) == "" && strings.TrimSpace(sc.Agent) != "" {
		loaded, status, loadErr := u.loadAgentDefinition(ws, sc.Agent)
		if loadErr != nil {
			if strings.TrimSpace(sc.Loop) == "" {
				writeErr(w, status, loadErr.Error())
				return
			}
		} else {
			if strings.TrimSpace(sc.Loop) == "" && strings.TrimSpace(loaded.loopSrc) != "" {
				sc.LoopSource = loaded.loopSrc
				if loaded.agent.Loop != "" {
					sc.Loop = loaded.agent.Loop
				} else {
					sc.Loop = sc.Agent + ".ts"
				}
			}
			if strings.TrimSpace(sc.Instructions) == "" {
				sc.Instructions = loaded.instrSrc
			}
			if strings.TrimSpace(sc.Model) == "" {
				sc.Model = loaded.agent.Model
			}
			if strings.TrimSpace(sc.Safety) == "" && (loaded.agent.Safety == "default" || loaded.agent.Safety == "none") {
				sc.Safety = loaded.agent.Safety
			}
		}
	}
	if sc.Timeout != "" {
		if d, err := time.ParseDuration(sc.Timeout); err == nil && d > 30*time.Second {
			sc.Timeout = "30s"
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := looptest.RunWithResult(ctx, sc)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"result": res,
	})
}

// handleStudioApprovals returns pending execution approvals visible to Studio,
// optionally filtered by session_key.
func (u *UI) handleStudioApprovals(w http.ResponseWriter, r *http.Request) {
	if u.deps.Guard == nil {
		writeErr(w, http.StatusServiceUnavailable, "execution approval store is not enabled on this instance")
		return
	}
	sessionKey := strings.TrimSpace(r.URL.Query().Get("session_key"))
	items, err := u.deps.Guard.ListPending(r.Context(), sessionKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if items == nil {
		items = []execguard.ApprovalRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"approvals": items,
	})
}

// handleStudioApprovalDecision records an authenticated Studio operator's
// approve or deny decision on one pending runtime approval record.
func (u *UI) handleStudioApprovalDecision(w http.ResponseWriter, r *http.Request) {
	if u.deps.Guard == nil {
		writeErr(w, http.StatusServiceUnavailable, "execution approval store is not enabled on this instance")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing approval id")
		return
	}
	var body struct {
		Approve  *bool  `json:"approve"`
		Decision string `json:"decision"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var approve bool
	switch {
	case body.Approve != nil:
		approve = *body.Approve
	case body.Decision != "":
		switch strings.ToLower(strings.TrimSpace(body.Decision)) {
		case "approve", "approved", "yes":
			approve = true
		case "deny", "denied", "no", "reject":
			approve = false
		default:
			writeErr(w, http.StatusBadRequest, "decision must be approve or deny")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "approve or decision field is required")
		return
	}

	rec, err := u.deps.Guard.Decide(r.Context(), id, approve, "studio:operator", studio.ChannelName)
	if err != nil {
		switch {
		case errors.Is(err, execguard.ErrApprovalNotFound):
			writeErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, execguard.ErrApprovalExpired), errors.Is(err, execguard.ErrApprovalNotPending):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, execguard.ErrUnauthorizedApprover):
			writeErr(w, http.StatusForbidden, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"approval": rec,
	})
}

type studioEventView struct {
	runtime.JournalEntry
	HasTrace bool   `json:"has_trace"`
	Outcome  string `json:"outcome"`
}

func studioEventOutcome(e runtime.JournalEntry) string {
	status := strings.ToLower(strings.TrimSpace(e.Status))
	switch {
	case strings.HasPrefix(status, "dropped"), strings.HasPrefix(status, "blocked"):
		return "dropped"
	case status == "ignored":
		return "ignored"
	case status == "failed", e.Err != "" && status != "ignored":
		return "failed"
	case status == "answered", status == "delivered", e.Direction == "out" && status == "":
		return "answered"
	default:
		return "received"
	}
}

// handleStudioEvents returns recent message journal entries annotated with
// Event Center outcomes and trace-ring availability for live diagnostics.
func (u *UI) handleStudioEvents(w http.ResponseWriter, r *http.Request) {
	if u.deps.Journal == nil {
		writeErr(w, http.StatusServiceUnavailable, "message journal is not enabled on this instance")
		return
	}
	q := r.URL.Query()
	limit := 100
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if n > 500 {
			n = 500
		}
		limit = n
	}
	var since, until int64
	if raw := strings.TrimSpace(q.Get("since")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid since timestamp")
			return
		}
		since = n
	}
	if raw := strings.TrimSpace(q.Get("until")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid until timestamp")
			return
		}
		until = n
	}

	statusParam := strings.TrimSpace(q.Get("status"))
	f := runtime.JournalFilter{
		Channel:   strings.TrimSpace(q.Get("channel")),
		Agent:     strings.TrimSpace(q.Get("agent")),
		Chat:      strings.TrimSpace(q.Get("chat")),
		SessionID: strings.TrimSpace(q.Get("session")),
		Direction: strings.TrimSpace(q.Get("direction")),
		Since:     since,
		Until:     until,
		Limit:     limit,
	}
	postFilterOutcome := ""
	switch statusParam {
	case "dropped", "answered", "failed", "received":
		postFilterOutcome = statusParam
		if fetchLimit := limit * 4; fetchLimit > f.Limit {
			if fetchLimit > 1000 {
				fetchLimit = 1000
			}
			f.Limit = fetchLimit
		}
	case "":
	default:
		f.Status = statusParam
	}

	entries, err := u.deps.Journal.ListMessages(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]studioEventView, 0, len(entries))
	for _, e := range entries {
		outcome := studioEventOutcome(e)
		if postFilterOutcome != "" && outcome != postFilterOutcome {
			continue
		}
		hasTrace := u.deps.Studio != nil && e.SessionID != "" && u.deps.Studio.HasTrace(e.SessionID)
		out = append(out, studioEventView{
			JournalEntry: e,
			HasTrace:     hasTrace,
			Outcome:      outcome,
		})
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"events": out,
	})
}

// handleStudioSessionTrace returns buffered execution trace events for any
// runtime session key (Telegram, Webhook, Studio, etc.).
func (u *UI) handleStudioSessionTrace(w http.ResponseWriter, r *http.Request) {
	if u.deps.Studio == nil {
		writeErr(w, http.StatusServiceUnavailable, "studio broker is not enabled on this instance")
		return
	}
	sessionKey := strings.TrimSpace(r.URL.Query().Get("session"))
	if sessionKey == "" {
		writeErr(w, http.StatusBadRequest, "session query parameter is required")
		return
	}
	events := u.deps.Studio.SessionTraceSnapshot(sessionKey)
	if events == nil {
		events = []session.TraceEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"session": sessionKey,
		"events":  events,
	})
}
