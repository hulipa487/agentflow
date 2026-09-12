// Package webui is the operator console: an embedded single-page UI plus a
// JSON API, mounted on the admin server (internal/core/metrics). It covers
// per-tenant API keys (the credential store), hot model management with
// explicit persist-to-config, raw config editing with boot-equivalent
// validation, and live metrics/session views.
//
// Security posture: the API requires the admin bearer token on every route
// (the caller mounts it with requireAuth); the static files are inert without
// it. Secrets are write-only — list endpoints return metadata and last-4
// fingerprints, never values.
package webui

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/credentials"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/drivers/llm"
)

//go:embed static
var staticFS embed.FS

//go:embed docs
var docsFS embed.FS

// Deps are the runtime handles the console needs. All are wired in main; Creds
// and Logs may be nil (credentials disabled / log tail off).
type Deps struct {
	ConfigPath string
	Cfg        *config.Config
	Models     *llm.Manager
	History    *metrics.History
	Creds      *credentials.Store
	Logs       *LogRing
	Snapshot   func() ([]supervisor.SessionStatus, int, int)
	Version    string
	StartedAt  time.Time
}

// UI is the console. Static serves the SPA (unauthenticated — it is inert
// without the API); API serves /admin/api/* and must be mounted with auth.
type UI struct {
	deps Deps
}

func New(d Deps) *UI { return &UI{deps: d} }

// Static serves the embedded SPA files. Only known asset paths resolve;
// everything else 404s (no directory traversal, no open redirect).
func (u *UI) Static() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		switch path {
		case "index.html", "app.js", "styles.css", "shared/tokens.css", "shared/theme.js":
		default:
			http.NotFound(w, r)
			return
		}
		b, err := fs.ReadFile(sub, path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch {
		case strings.HasSuffix(path, ".html"):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case strings.HasSuffix(path, ".js"):
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case strings.HasSuffix(path, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		// The SPA is per-deployment and never cached across versions.
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
}

// Docs serves the embedded documentation site at /docs/. Like Static, it is
// unauthenticated (documentation is not sensitive) and path-whitelisted.
func (u *UI) Docs() http.Handler {
	sub, err := fs.Sub(docsFS, "docs")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/docs/")
		if path == "" || path == "docs" {
			path = "index.html"
		}
		switch path {
		case "index.html", "css/styles.css", "js/nav.js":
		default:
			http.NotFound(w, r)
			return
		}
		b, err := fs.ReadFile(sub, path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch {
		case strings.HasSuffix(path, ".html"):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case strings.HasSuffix(path, ".js"):
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case strings.HasSuffix(path, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
}

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
	mux.HandleFunc("GET /admin/api/config", u.handleConfigGet)
	mux.HandleFunc("POST /admin/api/config/validate", u.handleConfigValidate)
	mux.HandleFunc("POST /admin/api/config/save", u.handleConfigSave)
	mux.HandleFunc("GET /admin/api/metrics", u.handleMetrics)
	mux.HandleFunc("GET /admin/api/logs", u.handleLogs)
	mux.HandleFunc("GET /admin/api/credentials/users", u.handleCredentialUsers)
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
	for name, a := range d.Cfg.Agents {
		agents = append(agents, map[string]any{
			"name":       name,
			"model":      a.Model,
			"loop":       a.Loop,
			"persistent": a.Persistent,
			"singleton":  a.Singleton,
		})
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
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"version":     d.Version,
		"started_at":  d.StartedAt.Unix(),
		"uptime_s":    int64(time.Since(d.StartedAt).Seconds()),
		"config_path": d.ConfigPath,
		"agents":      agents,
		"channels":    channels,
		"sessions":    sessions,
		"sessions_active": active,
		"sessions_idle":   idle,
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
