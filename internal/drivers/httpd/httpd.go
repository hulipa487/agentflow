// Package httpd is the shared HTTP server that all webhook-style channel drivers
// attach to. Instead of each driver spinning up its own http.Server on its own
// port (which forced a unique port per channel and clashed when two wanted the
// same default), every HTTP channel registers its path on this one mux. The
// listener is started once, in main, and shut down once.
//
// It also owns two cross-cutting concerns:
//   - a built-in GET /health endpoint, used both as the runtime's own startup
//     self-probe and as the public reverse-proxy health target.
//   - Probe, which GETs a public base URL's /health to decide whether the
//     deployment is externally reachable (telelgram's "auto" mode uses this to
//     pick webhook vs polling).
package httpd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// netListen is the bound-listener seam; overridable in tests via netListenVar.
// Binding up front (rather than letting Serve do it) surfaces bind errors
// synchronously from Start so the caller can fail fast instead of discovering
// a bad address from a goroutine log line.
var netListen = func(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

// Server wraps a single http.Server backed by one ServeMux that every channel
// driver registers its path on.
type Server struct {
	listen string
	log    *slog.Logger
	mux    *http.ServeMux
	srv    *http.Server

	// paths tracks registered routes so Handle can fail loudly on duplicates
	// (a silent collision would mean one channel shadows another's handler and
	// the bug only shows up at request time).
	paths map[string]bool
}

// New builds a server bound to listen. It does not start listening until Start.
// A /health handler is registered up front. A nil log is fine (used in tests).
func New(listen string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	mux := http.NewServeMux()
	s := &Server{
		listen: listen,
		log:    log.With("module", "httpd"),
		mux:    mux,
		paths:  map[string]bool{},
	}
	s.Handle("/health", s.handleHealth)
	return s
}

// Handle registers a handler at path. A duplicate path is a programming error
// (two channels on the same route) and panics loudly rather than shadowing.
// Paths ending in "/" are mounted as subtrees (matching /path/anything) by the
// stdlib mux; that's how /webhook/<chan>/<uuid>/ is exposed without enumerating
// every sub-path.
func (s *Server) Handle(path string, fn http.HandlerFunc) {
	if path == "" {
		panic("httpd: empty path")
	}
	if s.paths[path] {
		panic("httpd: duplicate path " + path)
	}
	s.paths[path] = true
	s.mux.HandleFunc(path, fn)
}

func (s *Server) Listen() string { return s.listen }

// Handler returns the underlying mux, so embedders and tests can serve it
// through their own server instead of Start (e.g. httptest.NewServer).
func (s *Server) Handler() http.Handler { return s.mux }

// Start begins serving. It returns once the server is bound; ListenAndServe
// runs in a goroutine so callers can proceed to wire the rest of the runtime.
func (s *Server) Start() error {
	s.srv = &http.Server{
		Addr:              s.listen,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := netListen(s.listen)
	if err != nil {
		return err
	}
	s.log.Info("listening", "addr", s.listen)
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("server died", "err", err)
		}
	}()
	return nil
}

// Stop shuts the shared server down, draining in-flight requests for the ctx.
func (s *Server) Stop(ctx context.Context) {
	if s.srv != nil {
		_ = s.srv.Shutdown(ctx)
	}
}

// handleHealth returns 200 {"ok":true}. Unauthenticated by design — it is the
// probe target and the proxy health check. No request body logging.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// ProbeResult is the outcome of a Probe call, reported so callers (telegram
// auto mode) and logs can cite both verdict and latency.
type ProbeResult struct {
	OK      bool
	Latency time.Duration
	Err     error
}

// Probe GETs <publicURL>/health with a short timeout to test whether the
// deployment is externally reachable. publicURL is a scheme+host base with no
// path (e.g. "https://agentflow.example.com"). An empty publicURL returns
// OK=false immediately (no network attempt) — callers treat that as "no public
// URL configured" rather than an outage.
//
// A 5s deadline caps the whole attempt; any non-2xx status or transport error
// is OK=false with Err populated. This is best-effort reachability, not a
// correctness guarantee — /health returning 200 is the spec the public proxy
// must satisfy for webhook mode to engage.
func Probe(ctx context.Context, publicURL string) ProbeResult {
	if publicURL == "" {
		return ProbeResult{OK: false, Err: errors.New("no public url configured")}
	}
	base := strings.TrimRight(publicURL, "/")
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return ProbeResult{OK: false, Err: err, Latency: time.Since(start)}
	}
	resp, err := http.DefaultClient.Do(req)
	lat := time.Since(start)
	if err != nil {
		return ProbeResult{OK: false, Err: err, Latency: lat}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ProbeResult{OK: false, Err: fmt.Errorf("health status %d", resp.StatusCode), Latency: lat}
	}
	return ProbeResult{OK: true, Latency: lat}
}
