// Package users serves the user-facing profile API on the shared public HTTP
// listener: profile registration, channel linking, and self-service views.
//
// It is designed to be driven by a frontend the engine does not ship. Nothing
// here renders HTML; the contract is JSON over HTTP, and authentication is a
// bearer token in every case — either an access token from the deployment's
// identity provider, verified against that issuer's published keys, or one of
// the per-profile API tokens this package issues. There is no cookie and no
// server-side login, so there is nothing ambient for another site to replay and
// no CSRF token to carry.
//
// Two proofs keep a profile honest. The caller proves which profile it is with
// the token it holds; it proves a channel handle by echoing a one-time challenge
// back from that handle. Neither half alone links anything.
package users

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/files"
	"agentflow/internal/core/identity"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/oidc"
	"agentflow/internal/core/runtime"
)

// Prefix is the subtree this API owns on the shared HTTP listener.
const Prefix = "/v1/users"

// maxBody caps request bodies: every field here is a short string, so a larger
// body is a mistake or an attack.
const maxBody = 8 << 10

// Options are the runtime handles the API needs beyond configuration.
type Options struct {
	// LinkableChannels are the configured channels whose sender identity the
	// platform authenticates — the ones a profile may actually link. Reporting
	// an unlinkable channel would send a user to a flow that cannot work.
	LinkableChannels []string
	// Files answers /me/projects. Nil means the file store is disabled.
	Files *files.Manager
	// Store answers /me/usage. Nil means accounting is disabled.
	Store runtime.Ledger
	// Quota reports the caller's limit. Nil means no quota is configured.
	Quota QuotaStatus
	// Verifier checks access tokens minted by the deployment's identity
	// provider. Nil means no provider is configured, and only API tokens work.
	Verifier *oidc.Verifier
	// JITProvisioning creates a profile on a verified first login.
	JITProvisioning bool
}

// QuotaStatus is the quota's read surface, as the API needs it.
type QuotaStatus interface {
	Status(userID string) (used, limit, inFlight int64, err error)
}

// API serves the user profile endpoints.
type API struct {
	reg      *identity.Registry
	log      *slog.Logger
	cfg      config.UsersConfig
	mode     string // "open" | "invite"
	linkTTL  time.Duration
	verifier *oidc.Verifier
	jit      bool
	linkable []string
	files    *files.Manager
	store    runtime.Ledger
	quota    QuotaStatus
	limiter  *ipLimiter
}

// New builds the API. mode selects whether registration is open or requires an
// operator-issued invite.
func New(reg *identity.Registry, cfg config.UsersConfig, log *slog.Logger, opts Options) *API {
	return &API{
		reg:      reg,
		log:      log.With("module", "users"),
		cfg:      cfg,
		mode:     cfg.RegistrationMode(),
		linkTTL:  cfg.LinkChallengeTTL(),
		verifier: opts.Verifier,
		jit:      opts.JITProvisioning,
		linkable: opts.LinkableChannels,
		files:    opts.Files,
		store:    opts.Store,
		quota:    opts.Quota,
		// Registration and the public endpoints are the anonymous surface; they
		// share one modest per-address budget.
		limiter: newIPLimiter(30, 10),
	}
}

// Handler returns the /v1/users subtree handler, for mounting on the shared
// listener with httpd.Server.Handle(Prefix+"/", api.Handler()).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(Prefix+"/config", a.public(a.handleConfig))
	mux.HandleFunc(Prefix+"/register", a.limited(a.handleRegister))
	mux.HandleFunc(Prefix+"/me", a.handleMe)
	mux.HandleFunc(Prefix+"/me/", a.handleMeSubtree)
	return a.cors(mux)
}

// --- CORS -------------------------------------------------------------------

// cors answers cross-origin requests for origins an operator has named. With no
// configured origins the API sends no CORS headers at all: a browser on another
// origin then cannot call it, which is the safe default.
func (a *API) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && a.cfg.OriginAllowed(origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			// Deliberately no Allow-Credentials: requests carry a bearer token,
			// never a cookie, so a browser has no ambient authority to lend to
			// another origin.
			h.Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		} else if r.Method == http.MethodOptions {
			// A preflight from an unknown origin: answer it plainly and without
			// CORS headers, so the browser refuses the actual request rather
			// than us having to guess.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- endpoints --------------------------------------------------------------

// handleConfig is the one public endpoint a frontend calls first: it says
// whether registration is open or invite-only, and which channels can be
// linked here, so the UI renders a flow that can actually succeed.
func (a *API) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"registration":      a.mode,
		"linkable_channels": a.linkable,
		// How a frontend should authenticate: an access token from the
		// deployment's identity provider when one is configured, or an API
		// token from /register. Both go in Authorization: Bearer.
		"identity_provider": a.verifier != nil,
	})
}

type registerRequest struct {
	Invite      string `json:"invite"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

type registerResponse struct {
	UserID string      `json:"user_id"`
	Token  string      `json:"token"`
	Me     profileView `json:"profile"`
}

// handleRegister creates a profile and returns its API token exactly once. It is
// for scripts and for deployments with no identity provider; a deployment that
// has one does not need it, because logins provision profiles themselves.
func (a *API) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req registerRequest
	if !readJSON(w, r, &req) {
		return
	}
	if a.mode == "invite" {
		if req.Invite == "" {
			writeErr(w, http.StatusForbidden, "registration is invite-only: an invite code is required")
			return
		}
		if err := a.reg.RedeemInvite(req.Invite); err != nil {
			writeErr(w, http.StatusForbidden, "invite rejected: "+err.Error())
			return
		}
	}
	p, err := a.reg.CreateProfile(req.DisplayName, req.Email)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not create profile")
		a.log.Error("registration failed", "err", err)
		return
	}
	tok, err := a.reg.IssueToken(p.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not issue token")
		a.log.Error("token issue failed", "err", err, "user_id", p.UserID)
		return
	}
	metrics.Inc("agentflow_user_registrations")
	a.log.Info("user registered", "user_id", p.UserID, "mode", a.mode)

	writeJSON(w, http.StatusCreated, registerResponse{UserID: p.UserID, Token: tok, Me: viewOf(p)})
}

func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	auth, ok := a.requireAuth(w, r)
	if !ok {
		return
	}
	userID := auth.userID
	switch r.Method {
	case http.MethodGet:
		p, ok, err := a.reg.Get(userID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not read profile")
			return
		}
		if !ok {
			writeErr(w, http.StatusNotFound, "no such profile")
			return
		}
		refs, err := a.reg.Tokens(userID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not read tokens")
			return
		}
		out := viewOf(p)
		out.Tokens = refs
		writeJSON(w, http.StatusOK, out)

	case http.MethodPatch:
		var req struct {
			DisplayName *string `json:"display_name"`
			Email       *string `json:"email"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if err := a.reg.Update(userID, req.DisplayName, req.Email, nil); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		p, _, _ := a.reg.Get(userID)
		writeJSON(w, http.StatusOK, viewOf(p))

	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET or PATCH required")
	}
}

// handleMeSubtree serves /me/links, /me/links/{id}, /me/token/rotate,
// /me/usage and /me/projects.
func (a *API) handleMeSubtree(w http.ResponseWriter, r *http.Request) {
	auth, ok := a.requireAuth(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, Prefix+"/me/")
	switch {
	case rest == "links":
		a.handleLinks(w, r, auth.userID)
	case strings.HasPrefix(rest, "links/"):
		a.handleUnlink(w, r, auth.userID, strings.TrimPrefix(rest, "links/"))
	case rest == "token/rotate":
		a.handleRotate(w, r, auth.userID, auth)
	case rest == "usage":
		a.handleUsage(w, r, auth.userID)
	case rest == "projects":
		a.handleProjects(w, r, auth.userID)
	default:
		writeErr(w, http.StatusNotFound, "no such endpoint")
	}
}

type linkRequest struct {
	Channel string `json:"channel"`
}

type linkResponse struct {
	Channel      string `json:"channel"`
	Code         string `json:"code"`
	ExpiresAt    int64  `json:"expires_at"`
	Instructions string `json:"instructions"`
}

// handleLinks starts a challenge: the profile owner asks to link a handle on a
// channel, and gets a code to send from that handle.
func (a *API) handleLinks(w http.ResponseWriter, r *http.Request, userID string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req linkRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Channel == "" {
		writeErr(w, http.StatusBadRequest, "channel is required")
		return
	}
	code, expires, err := a.reg.StartLink(userID, req.Channel, a.linkTTL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	metrics.Inc("agentflow_user_link_challenges")
	writeJSON(w, http.StatusCreated, linkResponse{
		Channel:   req.Channel,
		Code:      code,
		ExpiresAt: expires,
		Instructions: fmt.Sprintf(
			"Send \"%s\" from the %s account you want to link. The link completes when that message arrives.",
			"/link "+code, req.Channel),
	})
}

// handleUnlink detaches a handle from the profile.
func (a *API) handleUnlink(w http.ResponseWriter, r *http.Request, userID, identityID string) {
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "DELETE required")
		return
	}
	if identityID == "" {
		writeErr(w, http.StatusBadRequest, "identity id is required")
		return
	}
	// Ownership is the authorization: Unlink only acts on identities that
	// belong to this profile.
	if err := a.reg.Unlink(userID, identityID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRotate issues a fresh token and revokes the one that authorized the
// call, so a leaked token can be retired without losing access. A cookie
// session has no token to rotate, so this is a bearer-only operation.
func (a *API) handleRotate(w http.ResponseWriter, r *http.Request, userID string, auth authResult) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	tok, err := a.reg.IssueToken(userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not issue token")
		return
	}
	if old := bearerToken(r); old != "" {
		if err := a.reg.RevokeToken(userID, old); err != nil {
			// The new token is live either way; say so rather than failing.
			a.log.Warn("rotate: old token not revoked", "err", err, "user_id", userID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

// handleUsage reports what the caller has spent: today's totals, a short daily
// trend, and where their quota stands.
func (a *API) handleUsage(w http.ResponseWriter, r *http.Request, userID string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	if a.store == nil {
		writeErr(w, http.StatusServiceUnavailable, "accounting is not enabled")
		return
	}
	today, err := a.store.UsageForDay(userID, runtime.DayKey(time.Now()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read usage")
		return
	}
	history, err := a.store.UsageHistory(userID, 7)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read usage history")
		return
	}
	out := map[string]any{
		"day":     runtime.DayKey(time.Now()),
		"today":   today,
		"history": history,
	}
	if a.quota != nil {
		used, limit, inFlight, err := a.quota.Status(userID)
		if err == nil {
			out["quota"] = map[string]any{
				"used": used, "limit": limit, "in_flight": inFlight,
				"unlimited": limit == 0,
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleProjects lists the caller's own file projects. The scope is resolved
// from the profile id, so a user can only ever see their own.
func (a *API) handleProjects(w http.ResponseWriter, r *http.Request, userID string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	if a.files == nil {
		writeErr(w, http.StatusServiceUnavailable, "the file store is not enabled")
		return
	}
	projects, err := a.files.Projects(r.Context(), "user:"+userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list projects")
		return
	}
	if projects == nil {
		// An empty listing is [], not null: a frontend should not have to
		// special-case "no projects yet".
		projects = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

// --- auth -------------------------------------------------------------------

// authResult is how a request proved who it is.
type authResult struct {
	userID string
	// viaProvider marks an identity-provider login rather than an API token.
	viaProvider bool
}

// requireAuth resolves the caller from a bearer token. Two kinds are accepted:
// an API token issued by this package (afu_…), or an access token from the
// deployment's identity provider, verified against the issuer's published keys.
// There is no third path — no cookie, and so nothing ambient to replay.
func (a *API) requireAuth(w http.ResponseWriter, r *http.Request) (authResult, bool) {
	tok := bearerToken(r)
	if tok == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized: send Authorization: Bearer <token>")
		return authResult{}, false
	}
	if !strings.HasPrefix(tok, "afu_") {
		return a.authProvider(w, r, tok)
	}
	userID, ok := a.reg.TokenUser(tok)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return authResult{}, false
	}
	return authResult{userID: userID}, true
}

// authProvider verifies an identity-provider access token and resolves it to a
// profile, creating one on first login when just-in-time provisioning is on.
func (a *API) authProvider(w http.ResponseWriter, r *http.Request, token string) (authResult, bool) {
	if a.verifier == nil {
		writeErr(w, http.StatusUnauthorized, "this deployment has no identity provider configured; use an API token")
		return authResult{}, false
	}
	claims, err := a.verifier.Verify(r.Context(), token)
	if err != nil {
		a.log.Warn("access token rejected", "err", err)
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return authResult{}, false
	}
	id, created, err := a.reg.ProvisionOIDC(
		claims.Identity(a.verifier.Issuer()), claims.Name, claims.Email, a.jit)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return authResult{}, false
	}
	if created {
		metrics.Inc("agentflow_user_provisions")
	}
	return authResult{userID: id.UserID, viaProvider: true}, true
}

// --- middleware -------------------------------------------------------------

// public applies the rate limit without requiring authentication (register,
// session start, config).
func (a *API) public(next http.HandlerFunc) http.HandlerFunc { return a.limited(next) }

// limited applies the per-address budget to the endpoints an anonymous caller
// can reach.
func (a *API) limited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.limiter.allow(clientIP(r)) {
			writeErr(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if rest, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(rest)
	}
	return ""
}

// clientIP is the peer address. Behind a reverse proxy this is the proxy, so a
// deployment that fronts the listener must rate limit there as well — see the
// note on trusted proxies in the docs.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- views ------------------------------------------------------------------

type identityView struct {
	ID          string `json:"id"`
	Channel     string `json:"channel"`
	NativeFrom  string `json:"native_from"`
	Username    string `json:"username,omitempty"`
	Name        string `json:"name,omitempty"`
	Trust       string `json:"trust"`
	Deliverable bool   `json:"deliverable"`
	Linkable    bool   `json:"linkable"`
	FirstSeen   int64  `json:"first_seen"`
	LastSeen    int64  `json:"last_seen"`
}

type profileView struct {
	UserID       string              `json:"user_id"`
	DisplayName  string              `json:"display_name,omitempty"`
	Email        string              `json:"email,omitempty"`
	TokensPerDay int64               `json:"tokens_per_day"`
	CreatedAt    int64               `json:"created_at"`
	Identities   []identityView      `json:"identities"`
	Tokens       []identity.TokenRef `json:"tokens,omitempty"`
}

func viewOf(p identity.Profile) profileView {
	v := profileView{
		UserID:       p.UserID,
		DisplayName:  p.DisplayName,
		Email:        p.Email,
		TokensPerDay: p.TokensPerDay,
		CreatedAt:    p.CreatedAt,
		Identities:   make([]identityView, 0, len(p.Identities)),
	}
	for _, id := range p.Identities {
		v.Identities = append(v.Identities, identityView{
			ID: id.ID, Channel: id.Channel, NativeFrom: id.NativeFrom,
			Username: id.Username, Name: id.Name, Trust: id.Trust,
			Deliverable: id.Deliverable, Linkable: id.Linkable,
			FirstSeen: id.FirstSeen, LastSeen: id.LastSeen,
		})
	}
	return v
}

// --- plumbing ---------------------------------------------------------------

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read body")
		return false
	}
	if len(body) > maxBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "body too large")
		return false
	}
	if len(body) == 0 {
		return true // an empty body is valid: every field is optional
	}
	if err := json.Unmarshal(body, dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// --- rate limiting ----------------------------------------------------------

// ipLimiter is a token bucket per client address. The user API is reachable by
// anyone who can reach the listener, and registration writes a row, so the
// cheap abuse is flooding it.
type ipLimiter struct {
	mu      sync.Mutex
	perMin  float64
	burst   float64
	now     func() time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newIPLimiter(perMin, burst float64) *ipLimiter {
	return &ipLimiter{
		perMin:  perMin,
		burst:   burst,
		now:     time.Now,
		buckets: map[string]*bucket{},
	}
}

func (l *ipLimiter) allow(ip string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// Bound the map: an attacker rotating source addresses must not grow it
	// without limit.
	if len(l.buckets) > 4096 {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}

	b, ok := l.buckets[ip]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.perMin / 60
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
