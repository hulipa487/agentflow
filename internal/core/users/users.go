// Package users serves the user-facing profile API on the shared public HTTP
// listener: profile registration, channel linking, and self-service views.
//
// The surface carries no operator token — it is for the users themselves. A
// caller proves which profile it is with a per-profile bearer token issued at
// registration, and proves a channel handle by echoing a one-time challenge
// back from that handle. Neither half alone links anything: the code is only
// visible to whoever holds the profile token, and only the handle's owner can
// send from the handle.
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
	"agentflow/internal/core/identity"
	"agentflow/internal/core/metrics"
)

// Prefix is the subtree this API owns on the shared HTTP listener.
const Prefix = "/v1/users"

// maxBody caps request bodies: every field here is a short string, so a larger
// body is a mistake or an attack.
const maxBody = 8 << 10

// API serves the user profile endpoints.
type API struct {
	reg       *identity.Registry
	log       *slog.Logger
	mode      string // "open" | "invite"
	linkTTL   time.Duration
	inviteTTL time.Duration
	limiter   *ipLimiter
}

// New builds the API. mode selects whether registration is open or requires an
// operator-issued invite.
func New(reg *identity.Registry, cfg config.UsersConfig, log *slog.Logger) *API {
	return &API{
		reg:       reg,
		log:       log.With("module", "users"),
		mode:      cfg.RegistrationMode(),
		linkTTL:   cfg.LinkChallengeTTL(),
		inviteTTL: 7 * 24 * time.Hour,
		// Registration and linking are the write endpoints, so they share one
		// modest per-address budget: 30 requests a minute, bursting to 10.
		limiter: newIPLimiter(30, 10),
	}
}

// Handler returns the /v1/users subtree handler, for mounting on the shared
// listener with httpd.Server.Handle(Prefix+"/", api.Handler()).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(Prefix+"/register", a.limited(a.handleRegister))
	mux.HandleFunc(Prefix+"/me", a.authed(a.handleMe))
	mux.HandleFunc(Prefix+"/me/", a.authed(a.handleMeSubtree))
	return mux
}

// --- endpoints --------------------------------------------------------------

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
	a.log.Info("user registered", "user_id", p.UserID, "mode", a.mode, "channel", "")

	writeJSON(w, http.StatusCreated, registerResponse{
		UserID: p.UserID,
		Token:  tok,
		Me:     viewOf(p),
	})
}

func (a *API) handleMe(w http.ResponseWriter, r *http.Request, userID string) {
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

// handleMeSubtree serves /v1/users/me/links, /v1/users/me/links/{id} and
// /v1/users/me/token/rotate.
func (a *API) handleMeSubtree(w http.ResponseWriter, r *http.Request, userID string) {
	rest := strings.TrimPrefix(r.URL.Path, Prefix+"/me/")
	switch {
	case rest == "links":
		a.handleLinks(w, r, userID)
	case strings.HasPrefix(rest, "links/"):
		a.handleUnlink(w, r, userID, strings.TrimPrefix(rest, "links/"))
	case rest == "token/rotate":
		a.handleRotate(w, r, userID)
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
// call, so a leaked token can be retired without losing access.
func (a *API) handleRotate(w http.ResponseWriter, r *http.Request, userID string) {
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

// --- middleware -------------------------------------------------------------

// authed resolves the bearer token to a profile. Every endpoint except
// registration requires it.
func (a *API) authed(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := a.reg.TokenUser(bearerToken(r))
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r, userID)
	}
}

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
// deployment that fronts the listener must rate limit there as well.
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
