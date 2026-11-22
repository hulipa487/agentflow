// Package adminapi serves the headless operator and AgentFlow Studio JSON/SSE
// API mounted under /admin/api/* on the admin server (internal/core/metrics).
// It covers runtime state, metrics, log tailing, hot model and agent
// management, config validation/editing, users, tenancy, credentials, channels,
// safety, memory profiles, and the Studio playground/trace broker.
//
// Security posture: every route requires the admin bearer token (mounted with
// requireAuth=true). Secrets are write-only — list endpoints return metadata
// and last-4 fingerprints, never values.
package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/bundle"
	"agentflow/internal/core/credentials"
	"agentflow/internal/core/fleet"
	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/studio"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/core/tenancy"
	"agentflow/internal/drivers/llm"
)

// Deps are the runtime handles the admin API needs. All are wired in main;
// Creds and Logs may be nil (credentials disabled / log tail off).
type Deps struct {
	ConfigPath string
	// ConfigDir is set instead of ConfigPath when the instance runs from a
	// config directory: such an instance has several fragments
	// rather than one file, so the Config tab and the model persist/revert
	// paths — all of which swap a single file — report a named reason instead
	// of editing the wrong one.
	ConfigDir string
	Cfg       *config.Config
	Models    *llm.Manager
	History   *metrics.History
	Creds     *credentials.Store
	Logs      *LogRing
	Snapshot  func() ([]supervisor.SessionStatus, int, int)
	Version   string
	StartedAt time.Time

	// Agents is the runtime agent registry, and BuildAgent resolves a
	// configured agent into a supervisor definition with the same recipe
	// boot uses — injected because that recipe closes over the memory, tool
	// and metering wiring that lives in main. Either nil means the console
	// was built without agent control; the routes answer 503 with a named
	// reason instead of panicking.
	Agents     *supervisor.Supervisor
	BuildAgent func(name string, a config.Agent) (*supervisor.AgentDef, error)

	// Instance identifies this process in a fleet: the lease owner id, which is
	// how the console (and the lease table) tell one instance from another.
	Instance string

	// Region names where this instance runs (runtime.region), empty when the
	// deployment has no regions. It is what an operator — or a load balancer
	// reading this endpoint — needs to know to send a session's traffic to the
	// region that owns it.
	Region string

	// Topology is the deployment tier ("single" or "regional"), and Zone is the
	// optional availability-zone label inside a region. Both are reported so an
	// operator can tell a fleet from a lone process without reading the config.
	Topology string
	Zone     string

	// Instances lists the live instance registry: every process currently using
	// this runtime store. Nil means the console was built without a registry
	// (tests, or a boot that has not opened the store yet) and the field is
	// reported empty rather than failing the whole state call.
	Instances func(context.Context) ([]fleet.Instance, error)

	// Bundle is the full-fidelity user migration service: profile, memory,
	// files and re-encrypted credentials, for moving one person between
	// regional cells. Nil answers 503 with a named reason.
	Bundle *bundle.Service

	// Users is the per-user surface: profiles, the usage ledger and the message
	// journal (both live in the runtime store), the quota, and the file store.
	// Each nil handle degrades with a named reason rather than a blank answer.
	Users UserDeps

	// Tenancy is the groups/pools/grants registry. Nil means the console was
	// built without tenancy; those routes answer 503 with a named reason.
	Tenancy *tenancy.Registry

	// Studio is the standalone app's engine side (trace rings + playground
	// correlation); Workspace is where its builder persists agents. Nil means
	// /admin/api/studio answers 503 with a named reason.
	Studio        *studio.Broker
	Workspace     *studio.Workspace
	Channels      *studio.Channels
	Safety        *studio.Safety
	Memory        *studio.Memory
	Plugins       *studio.Plugins
	Media         *media.Service
	StudioCatalog func() studio.Catalog
}

// UI serves the authenticated /admin/api/* endpoints used by AgentFlow Studio
// and automation clients.
type UI struct {
	deps Deps

	// studioApplied is the definition the builder applied live, per agent.
	// The boot config in Deps.Cfg is a snapshot from startup, so without this
	// a reload after Apply would show the pre-edit definition. It is
	// process-local on purpose: a restart falls back to the config or the
	// persisted fragment, which is the truth at that point.
	mu            sync.Mutex
	studioApplied map[string]config.Agent
}

func New(d Deps) *UI { return &UI{deps: d} }

// API routes /admin/api/*. Mount with requireAuth=true on the admin server.
func (u *UI) API() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/api/state", u.handleState)
	mux.HandleFunc("GET /admin/api/models", u.handleModelsList)
	mux.HandleFunc("PUT /admin/api/models/{name}", u.handleModelUpsert)
	mux.HandleFunc("DELETE /admin/api/models/{name}", u.handleModelRemove)
	mux.HandleFunc("POST /admin/api/models/{name}/test", u.handleModelTest)
	mux.HandleFunc("POST /admin/api/models/persist", u.handleModelsPersist)
	mux.HandleFunc("POST /admin/api/models/revert", u.handleModelsRevert)
	mux.HandleFunc("GET /admin/api/agents", u.handleAgentsList)
	mux.HandleFunc("PUT /admin/api/agents/{name}", u.handleAgentUpsert)
	mux.HandleFunc("DELETE /admin/api/agents/{name}", u.handleAgentRemove)
	mux.HandleFunc("GET /admin/api/groups", u.handleGroupsList)
	mux.HandleFunc("POST /admin/api/groups", u.handleGroupCreate)
	mux.HandleFunc("GET /admin/api/groups/{id}", u.handleGroupGet)
	mux.HandleFunc("POST /admin/api/groups/{id}/members", u.handleGroupAddMember)
	mux.HandleFunc("DELETE /admin/api/groups/{id}/members/{user}", u.handleGroupRemoveMember)
	mux.HandleFunc("PUT /admin/api/groups/{id}/master", u.handleGroupSetMaster)
	mux.HandleFunc("DELETE /admin/api/groups/{id}", u.handleGroupDisband)
	mux.HandleFunc("GET /admin/api/pools", u.handlePoolsList)
	mux.HandleFunc("POST /admin/api/pools", u.handlePoolCreate)
	mux.HandleFunc("GET /admin/api/pools/{id}/grants", u.handlePoolGrants)
	mux.HandleFunc("PUT /admin/api/pools/{id}/grants", u.handlePoolSetGrant)
	mux.HandleFunc("DELETE /admin/api/pools/{id}/grants", u.handlePoolRemoveGrant)
	mux.HandleFunc("GET /admin/api/config", u.handleConfigGet)
	mux.HandleFunc("POST /admin/api/config/validate", u.handleConfigValidate)
	mux.HandleFunc("POST /admin/api/config/save", u.handleConfigSave)
	mux.HandleFunc("GET /admin/api/metrics", u.handleMetrics)
	mux.HandleFunc("GET /admin/api/logs", u.handleLogs)
	mux.HandleFunc("GET /admin/api/credentials/users", u.handleCredentialUsers)
	mux.HandleFunc("GET /admin/api/credentials", u.handleStudioCredentials)
	mux.HandleFunc("POST /admin/api/credentials", u.handleStudioCredentialPut)
	mux.HandleFunc("DELETE /admin/api/credentials", u.handleStudioCredentialDelete)
	// Per-user profiles: who they are, what they spent, what they own, and what
	// they did.
	mux.HandleFunc("GET /admin/api/users", u.handleUsers)
	mux.HandleFunc("GET /admin/api/users/{id}", u.handleUser)
	mux.HandleFunc("POST /admin/api/users/{id}/limit", u.handleUserLimit)
	mux.HandleFunc("PUT /admin/api/users/{id}/settings", u.handleUserSettings)
	mux.HandleFunc("POST /admin/api/users/{id}/unlink", u.handleUserUnlink)
	mux.HandleFunc("POST /admin/api/users/invites", u.handleInviteIssue)
	mux.HandleFunc("GET /admin/api/users/{id}/export", u.handleUserExport)
	mux.HandleFunc("GET /admin/api/users/{id}/bundle", u.handleUserBundle)
	mux.HandleFunc("POST /admin/api/users/import", u.handleUserImport)
	mux.HandleFunc("DELETE /admin/api/users/{id}", u.handleUserDelete)
	mux.HandleFunc("GET /admin/api/usage", u.handleUsage)

	// Studio (standalone builder/playground/trace app).
	mux.HandleFunc("GET /admin/api/studio/catalog", u.handleStudioCatalog)
	mux.HandleFunc("GET /admin/api/studio/agents", u.handleStudioAgents)
	mux.HandleFunc("GET /admin/api/studio/agents/{name}", u.handleStudioAgentGet)
	mux.HandleFunc("POST /admin/api/studio/agents/validate", u.handleStudioValidate)
	mux.HandleFunc("PUT /admin/api/studio/agents/{name}", u.handleStudioAgentPut)
	mux.HandleFunc("DELETE /admin/api/studio/agents/{name}", u.handleStudioAgentDelete)
	mux.HandleFunc("POST /admin/api/studio/chat", u.handleStudioChat)
	mux.HandleFunc("GET /admin/api/studio/threads", u.handleStudioThreads)
	mux.HandleFunc("POST /admin/api/studio/threads/{thread}/stop", u.handleStudioStopThread)
	mux.HandleFunc("GET /admin/api/studio/trace/{agent}/{thread}", u.handleStudioTrace)
	mux.HandleFunc("POST /admin/api/studio/loop/test", u.handleStudioLoopTest)
	mux.HandleFunc("GET /admin/api/studio/media/{handle}", u.handleStudioMedia)
	mux.HandleFunc("GET /admin/api/studio/channels", u.handleStudioChannels)
	mux.HandleFunc("POST /admin/api/studio/channels/validate", u.handleStudioChannelValidate)
	mux.HandleFunc("POST /admin/api/studio/channels", u.handleStudioChannelCreate)
	mux.HandleFunc("PUT /admin/api/studio/channels/{index}", u.handleStudioChannelPut)
	mux.HandleFunc("DELETE /admin/api/studio/channels/{index}", u.handleStudioChannelDelete)
	mux.HandleFunc("GET /admin/api/studio/safety", u.handleStudioSafety)
	mux.HandleFunc("POST /admin/api/studio/safety/validate", u.handleStudioSafetyValidate)
	mux.HandleFunc("POST /admin/api/studio/safety/profiles", u.handleStudioSafetyProfileCreate)
	mux.HandleFunc("PUT /admin/api/studio/safety/profiles/{name}", u.handleStudioSafetyProfilePut)
	mux.HandleFunc("DELETE /admin/api/studio/safety/profiles/{name}", u.handleStudioSafetyProfileDelete)
	mux.HandleFunc("POST /admin/api/studio/safety/classifiers", u.handleStudioSafetyClassifierCreate)
	mux.HandleFunc("PUT /admin/api/studio/safety/classifiers/{name}", u.handleStudioSafetyClassifierPut)
	mux.HandleFunc("DELETE /admin/api/studio/safety/classifiers/{name}", u.handleStudioSafetyClassifierDelete)
	mux.HandleFunc("POST /admin/api/studio/safety/lua-filters", u.handleStudioSafetyLuaFilterCreate)
	mux.HandleFunc("PUT /admin/api/studio/safety/lua-filters/{name}", u.handleStudioSafetyLuaFilterPut)
	mux.HandleFunc("DELETE /admin/api/studio/safety/lua-filters/{name}", u.handleStudioSafetyLuaFilterDelete)
	mux.HandleFunc("GET /admin/api/studio/memory", u.handleStudioMemory)
	mux.HandleFunc("POST /admin/api/studio/memory/validate", u.handleStudioMemoryValidate)
	mux.HandleFunc("POST /admin/api/studio/memory/profiles", u.handleStudioMemoryProfileCreate)
	mux.HandleFunc("PUT /admin/api/studio/memory/profiles/{name}", u.handleStudioMemoryProfilePut)
	mux.HandleFunc("DELETE /admin/api/studio/memory/profiles/{name}", u.handleStudioMemoryProfileDelete)
	mux.HandleFunc("POST /admin/api/studio/memory/backends", u.handleStudioMemoryBackendCreate)
	mux.HandleFunc("PUT /admin/api/studio/memory/backends/{name}", u.handleStudioMemoryBackendPut)
	mux.HandleFunc("DELETE /admin/api/studio/memory/backends/{name}", u.handleStudioMemoryBackendDelete)
	mux.HandleFunc("GET /admin/api/studio/plugins", u.handleStudioPlugins)
	mux.HandleFunc("POST /admin/api/studio/plugins/validate", u.handleStudioPluginValidate)
	mux.HandleFunc("PUT /admin/api/studio/plugins/builtins/{name}", u.handleStudioBuiltinPut)
	mux.HandleFunc("DELETE /admin/api/studio/plugins/builtins/{name}", u.handleStudioBuiltinDelete)
	mux.HandleFunc("POST /admin/api/studio/plugins/packages", u.handleStudioPackageCreate)
	mux.HandleFunc("DELETE /admin/api/studio/plugins/packages/{name}", u.handleStudioPackageDelete)
	mux.HandleFunc("PUT /admin/api/studio/plugins/packages/{name}/files/{file}", u.handleStudioPackageFilePut)
	mux.HandleFunc("DELETE /admin/api/studio/plugins/packages/{name}/files/{file}", u.handleStudioPackageFileDelete)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

// handleState is the dashboard payload: identity, uptime, sessions, and the
// configured agents/channels (names and wiring only — no secrets).
func (u *UI) handleState(w http.ResponseWriter, r *http.Request) {
	d := u.deps
	agents := []map[string]any{}
	if d.Agents != nil {
		liveAgents := d.Agents.Agents()
		names := make([]string, 0, len(liveAgents))
		for name, def := range liveAgents {
			if def == nil || def.SpawnTemplate != nil {
				continue
			}
			names = append(names, name)
		}
		// Keep stable sorted order.
		for i := 1; i < len(names); i++ {
			for j := i; j > 0 && names[j] < names[j-1]; j-- {
				names[j], names[j-1] = names[j-1], names[j]
			}
		}
		for _, name := range names {
			def := liveAgents[name]
			model := ""
			if def.Info != nil {
				model = def.Info.Model
			}
			loop := def.LoopFile
			if loop == "" && d.Cfg != nil {
				loop = d.Cfg.Agents[name].Loop
			}
			agents = append(agents, map[string]any{
				"name":       name,
				"model":      model,
				"loop":       loop,
				"persistent": def.Persistent,
			})
		}
	} else if d.Cfg != nil {
		for name, a := range d.Cfg.Agents {
			agents = append(agents, map[string]any{
				"name":       name,
				"model":      a.Model,
				"loop":       a.Loop,
				"persistent": a.Persistent,
			})
		}
	}
	channels := []map[string]any{}
	for i, ch := range d.Cfg.Gateway.Channels {
		name := ch.Name
		if name == "" {
			name = ch.Type
		}
		channels = append(channels, map[string]any{
			"name":          name,
			"type":          ch.Type,
			"agent":         ch.Agent,
			"path":          ch.Path,
			"media_enabled": len(ch.Media.Allow) > 0,
			"index":         i,
		})
	}
	var rows []supervisor.SessionStatus
	var active, idle int
	if d.Snapshot != nil {
		rows, active, idle = d.Snapshot()
	}
	sessions := []map[string]any{}
	for _, s := range rows {
		sessions = append(sessions, map[string]any{
			"session_id": s.SessionID,
			"agent":      s.Agent,
			"parent_id":  s.ParentID,
			"busy":       s.Busy,
		})
	}
	// config_path reports what this instance actually runs from — a config
	// file, or the directory whose fragments it was built from. Reporting the
	// -config default in both cases is what made a config-directory instance look
	// like it was reading a file that does not exist.
	configPath := d.ConfigPath
	if d.ConfigDir != "" {
		configPath = d.ConfigDir
	}
	// The live registry, with this instance marked and peers whose config epoch
	// differs flagged: two instances of one fleet on different configs is the
	// difference an operator cannot see from a single console otherwise.
	instances := []map[string]any{}
	if d.Instances != nil {
		live, err := d.Instances(r.Context())
		if err == nil {
			for _, in := range live {
				instances = append(instances, map[string]any{
					"owner":                in.Owner,
					"region":               in.Region,
					"zone":                 in.Zone,
					"version":              in.Version,
					"topology":             in.Topology,
					"config_epoch":         in.ConfigEpoch,
					"config_epoch_differs": in.ConfigEpoch != d.Cfg.Epoch,
					"started_at":           in.StartedAt,
					"last_seen":            in.LastSeen,
					"self":                 in.Owner == d.Instance,
				})
			}
		}
	}
	topology := d.Topology
	if topology == "" {
		topology = "single"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"version":             d.Version,
		"started_at":          d.StartedAt.Unix(),
		"uptime_s":            int64(time.Since(d.StartedAt).Seconds()),
		"config_path":         configPath,
		"config_epoch":        d.Cfg.Epoch,
		"instance":            d.Instance,
		"region":              d.Region,
		"topology":            topology,
		"zone":                d.Zone,
		"instances":           instances,
		"agents":              agents,
		"channels":            channels,
		"sessions":            sessions,
		"sessions_active":     active,
		"sessions_idle":       idle,
		"credentials_enabled": d.Creds != nil,
	})
}

// handleMetrics returns the sampled counter series for sparklines plus the
// latest values for gauges.
func (u *UI) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if u.deps.History == nil {
		writeErr(w, http.StatusServiceUnavailable, "metrics history not enabled")
		return
	}
	series := u.deps.History.All()
	latest := map[string]int64{}
	for name, pts := range series {
		if len(pts) > 0 {
			latest[name] = pts[len(pts)-1].Value
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "series": series, "latest": latest})
}

// handleCredentialUsers lists the user UUIDs holding credentials, so the UI
// can offer per-user browsing without knowing UUIDs out of band.
func (u *UI) handleCredentialUsers(w http.ResponseWriter, r *http.Request) {
	if u.deps.Creds == nil {
		writeErr(w, http.StatusServiceUnavailable, "credentials not enabled")
		return
	}
	users, err := u.deps.Creds.ListUsers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "users": users})
}
