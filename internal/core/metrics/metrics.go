// Package metrics is a lightweight Prometheus-compatible metrics surface for
// AgentFlow. It exposes counters and an optional authenticated admin HTTP
// server with /healthz, /readyz, /metrics, and a read-only sessions endpoint.
//
// Metrics never expose message text, tool arguments, memory records,
// credentials, or secrets.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentflow/internal/core/credentials"
)

// Counter is an atomic int64 metric.
type Counter struct {
	name   string
	help   string
	value  atomic.Int64
}

func NewCounter(name, help string) *Counter {
	return &Counter{name: name, help: help}
}

func (c *Counter) Inc()          { c.value.Add(1) }
func (c *Counter) Add(n int64)   { c.value.Add(n) }
func (c *Counter) Value() int64  { return c.value.Load() }
func (c *Counter) Name() string  { return c.name }
func (c *Counter) Help() string  { return c.help }

// Registry holds all metrics.
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*Counter
}

func NewRegistry() *Registry {
	return &Registry{counters: map[string]*Counter{}}
}

func (r *Registry) Register(c *Counter) {
	r.mu.Lock()
	r.counters[c.Name()] = c
	r.mu.Unlock()
}

func (r *Registry) Get(name string) (*Counter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.counters[name]
	return c, ok
}

// global is the process-wide registry of default counters. Handlers across the
// runtime increment through this; the admin server renders it. Wired in main.
var global = NewRegistry()

func init() {
	for _, c := range DefaultCounters() {
		global.Register(c)
	}
}

// Global returns the process-wide counter registry.
func Global() *Registry { return global }

// Inc increments a named global counter by 1 (no-op if unregistered).
func Inc(name string) {
	if c, ok := global.Get(name); ok {
		c.Inc()
	}
}

// Add increments a named global counter by n (no-op if unregistered).
func Add(name string, n int64) {
	if c, ok := global.Get(name); ok {
		c.Add(n)
	}
}

// PrometheusFormat renders counters in Prometheus text exposition format.
func (r *Registry) PrometheusFormat() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var b strings.Builder
	for _, c := range r.counters {
		b.WriteString(fmt.Sprintf("# HELP %s %s\n", c.Name(), c.Help()))
		b.WriteString(fmt.Sprintf("# TYPE %s counter\n", c.Name()))
		b.WriteString(fmt.Sprintf("%s %d\n", c.Name(), c.Value()))
	}
	return b.String()
}

// DefaultCounters creates the standard set of runtime counters.
func DefaultCounters() map[string]*Counter {
	return map[string]*Counter{
		"agentflow_ingress_total":     NewCounter("agentflow_ingress_total", "Total inbound messages"),
		"agentflow_ingress_dropped":   NewCounter("agentflow_ingress_dropped", "Inbound messages dropped"),
		"agentflow_egress_total":      NewCounter("agentflow_egress_total", "Total outbound replies"),
		"agentflow_egress_failed":     NewCounter("agentflow_egress_failed", "Outbound replies that failed delivery"),
		"agentflow_sessions_active":   NewCounter("agentflow_sessions_active", "Currently active sessions (gauge)"),
		"agentflow_children_spawned":  NewCounter("agentflow_children_spawned", "Total spawned child agents"),
		"agentflow_children_died":     NewCounter("agentflow_children_died", "Total child agent deaths"),
		"agentflow_requests_pending":  NewCounter("agentflow_requests_pending", "Pending agent.request calls (gauge)"),
		"agentflow_timers_pending":    NewCounter("agentflow_timers_pending", "Pending timers (gauge)"),
		"agentflow_tool_confirmations": NewCounter("agentflow_tool_confirmations", "Tool confirmations requested"),
		"agentflow_llm_calls":         NewCounter("agentflow_llm_calls", "Total LLM calls"),
		"agentflow_llm_tokens":        NewCounter("agentflow_llm_tokens", "Total LLM tokens consumed"),
		"agentflow_budget_denied":     NewCounter("agentflow_budget_denied", "LLM calls denied by budget"),
		"agentflow_safety_drops":      NewCounter("agentflow_safety_drops", "Messages/replies dropped by safety"),
		"agentflow_channel_errors":   NewCounter("agentflow_channel_errors", "Channel delivery errors"),
	}
}

// AdminServer is the HTTP admin/metrics endpoint. It binds to loopback by
// default; remote binding requires a bearer token.
type AdminServer struct {
	srv       *http.Server
	reg       *Registry
	token     string
	readyz    func() bool
	sessions  func() []SessionInfo
	credStore *credentials.Store
	log       *slog.Logger
}

// SessionInfo is read-only session metadata for the admin endpoint.
type SessionInfo struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	ParentID  string `json:"parent_id,omitempty"`
}

// NewAdminServer creates the admin server. If token is non-empty, non-health
// endpoints require Authorization: Bearer <token>.
func NewAdminServer(addr, token string, reg *Registry, log *slog.Logger) *AdminServer {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &AdminServer{reg: reg, token: token, log: log.With("module", "admin")}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/metrics", s.auth(s.handleMetrics))
	mux.HandleFunc("/v1/sessions", s.auth(s.handleSessions))
	mux.HandleFunc("/admin/credentials", s.auth(s.handleCredentials))
	mux.HandleFunc("/admin/credentials/", s.auth(s.handleCredentialsDelete))
	s.srv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return s
}

// SetReady sets the readiness check. The runtime calls this once boot is complete.
func (s *AdminServer) SetReady(ready bool) {
	s.readyz = func() bool { return ready }
}

// SetSessions sets the session lister. The supervisor provides this.
func (s *AdminServer) SetSessions(lister func() []SessionInfo) {
	s.sessions = lister
}

// SetCredentials sets the credential store backing the /admin/credentials
// endpoints. The control plane (an onboarding service) provisions tenant API
// keys here; the agent loop can only consume them via http.request's `auth`.
func (s *AdminServer) SetCredentials(store *credentials.Store) {
	s.credStore = store
}

// Start begins listening. Blocks until shutdown.
func (s *AdminServer) Start() error {
	s.log.Info("admin server listening", "addr", s.srv.Addr)
	return s.srv.ListenAndServe()
}

// Stop gracefully shuts down.
func (s *AdminServer) Stop(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func (s *AdminServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got := r.Header.Get("Authorization")
			if got != "Bearer "+s.token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (s *AdminServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

func (s *AdminServer) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.readyz != nil && s.readyz() {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ready")
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprint(w, "not ready")
}

func (s *AdminServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprint(w, s.reg.PrometheusFormat())
}

func (s *AdminServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		json.NewEncoder(w).Encode([]SessionInfo{})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.sessions())
}

// credUnavailable writes 503 when the credential store is not enabled.
func (s *AdminServer) credUnavailable(w http.ResponseWriter) {
	http.Error(w, "credentials not enabled", http.StatusServiceUnavailable)
}

// handleCredentials provisions (POST), lists (GET), or revokes (DELETE) a
// tenant API key. Secret values are never returned in a response.
func (s *AdminServer) handleCredentials(w http.ResponseWriter, r *http.Request) {
	if s.credStore == nil {
		s.credUnavailable(w)
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req struct {
			User    string `json:"user"`
			Service string `json:"service"`
			Kind    string `json:"kind"`
			Secret  string `json:"secret"`
			Header  string `json:"header"`
			Scheme  string `json:"scheme"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		kind := req.Kind
		if kind == "" {
			kind = "api_key"
		}
		if err := s.credStore.Put(r.Context(), req.User, req.Service, kind, req.Secret, req.Header, req.Scheme); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"ok": true})

	case http.MethodGet:
		user := r.URL.Query().Get("user")
		if user == "" {
			http.Error(w, "missing user", http.StatusBadRequest)
			return
		}
		refs, err := s.credStore.List(r.Context(), user)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"credentials": refs})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleCredentialsDelete revokes a credential: DELETE /admin/credentials/{service}?user=<uuid>.
func (s *AdminServer) handleCredentialsDelete(w http.ResponseWriter, r *http.Request) {
	if s.credStore == nil {
		s.credUnavailable(w)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	service := strings.TrimPrefix(r.URL.Path, "/admin/credentials/")
	user := r.URL.Query().Get("user")
	if service == "" || user == "" {
		http.Error(w, "missing service or user", http.StatusBadRequest)
		return
	}
	if err := s.credStore.Delete(r.Context(), user, service); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
