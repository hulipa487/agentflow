// Package chatapi is the OpenAI-compatible channel driver (type: openai).
//
// It is a channel, not a proxy: it authenticates, parses, correlates the reply
// and speaks SSE, and makes no decision about meaning. Lua decides — the route
// handler picks the session (one per request, or one per conversation) and the
// loop decides what the request is (forward it to a model, enrich it, answer it
// as an agent). The default loop is a faithful pass-through, so a client that is
// itself an agent gets exactly a model.
//
// The request body does not cross the Lua bridge (D6). The message carries a
// small payload with a request_ref; the parsed request lives in the shared
// chatreq registry, where llm.forward reads it and openai.request exposes it on
// demand.
package chatapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"agentflow/internal/core/chatreq"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/drivers/httpd"
)

// Defaults. The timeout is the whole request, not one model call; a coding
// turn that runs tools can legitimately take minutes.
const (
	DefaultPath            = "/v1/"
	DefaultTimeout         = 600 * time.Second
	DefaultMaxRequestBytes = 32 << 20
)

// heartbeatInterval is how often an idle SSE stream sends a comment line so a
// proxy does not close it. A variable so a test can observe the beat.
var heartbeatInterval = 15 * time.Second

// Options is one openai channel's resolved configuration.
type Options struct {
	// Path is the prefix the two endpoints hang off; it ends with "/".
	Path string
	// Agents are the exposed agent names, in /v1/models order. The request's
	// model names one of them.
	Agents []string
	// Auth is user_token | key | none, already validated against the config.
	Auth string
	// Keys are the resolved shared keys for auth: key.
	Keys []string
	// TokenUser resolves a per-user bearer token for auth: user_token.
	TokenUser func(token string) (userID string, ok bool)
	// Timeout bounds one whole request.
	Timeout time.Duration
	// MaxRequestBytes bounds the POST body.
	MaxRequestBytes int
	// ExposeReasoning sends model reasoning as reasoning_content.
	ExposeReasoning bool
	// Ready reports whether the instance is serving; false answers 503.
	Ready func() bool
	// StopSession terminates one session (for a cancelled ephemeral request).
	StopSession func(sessionKey string)
	// Store and Policy are the channel media policy: a data-URL image is
	// ingested through them before it reaches a provider.
	Store  media.Store
	Policy media.Policy
	// Requests is the parsed-request registry shared with llm.forward.
	Requests *chatreq.Registry
}

// Driver serves the OpenAI endpoints and implements the gateway's delivery
// interfaces.
type Driver struct {
	name    string
	path    string
	agents  []string
	exposed map[string]bool
	auth    string
	keys    [][]byte
	opt     Options
	sink    router.Sink
	log     *slog.Logger

	seq atomic.Uint64

	mu      sync.Mutex
	pending map[string]*turn // reply id → running turn
	idem    map[string]*turn // Idempotency-Key hash → running turn
}

// New constructs the driver and mounts its routes on the shared listener.
func New(name string, opt Options, sink router.Sink, srv *httpd.Server, log *slog.Logger) *Driver {
	if opt.Path == "" {
		opt.Path = DefaultPath
	}
	if opt.Timeout <= 0 {
		opt.Timeout = DefaultTimeout
	}
	if opt.MaxRequestBytes <= 0 {
		opt.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if opt.Requests == nil {
		opt.Requests = chatreq.NewRegistry()
	}
	d := &Driver{
		name:    name,
		path:    opt.Path,
		agents:  append([]string(nil), opt.Agents...),
		exposed: make(map[string]bool, len(opt.Agents)),
		auth:    opt.Auth,
		opt:     opt,
		sink:    sink,
		log:     log.With("driver", "chatapi", "channel", name),
		pending: map[string]*turn{},
		idem:    map[string]*turn{},
	}
	for _, a := range opt.Agents {
		d.exposed[a] = true
	}
	for _, k := range opt.Keys {
		d.keys = append(d.keys, []byte(k))
	}
	srv.Handle(d.path, d.handleCatchAll)
	srv.Handle(d.path+"chat/completions", d.handleChat)
	srv.Handle(d.path+"models", d.handleModels)
	srv.Handle(d.path+"models/", d.handleModel)
	return d
}

func (d *Driver) Name() string { return d.name }

// lookup resolves the turn a reply belongs to.
func (d *Driver) lookup(replyTo string) *turn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pending[replyTo]
}

// Deliver implements gateway.Driver: session.send on this channel appends to
// the turn's content (3.4).
func (d *Driver) Deliver(replyTo, text string, attachments []media.Part) error {
	t := d.lookup(replyTo)
	if t == nil {
		return fmt.Errorf("openai channel %q has no pending request %q", d.name, replyTo)
	}
	return t.Deliver(replyTo, text, attachments)
}

// DeliverReply implements gateway.ReplyDriver.
func (d *Driver) DeliverReply(replyTo string, r gateway.Reply) error {
	t := d.lookup(replyTo)
	if t == nil {
		return fmt.Errorf("openai channel %q has no pending request %q", d.name, replyTo)
	}
	return t.DeliverReply(replyTo, r)
}

// DeliverDelta implements gateway.DeltaDriver.
func (d *Driver) DeliverDelta(replyTo, text string) error {
	t := d.lookup(replyTo)
	if t == nil {
		return fmt.Errorf("openai channel %q has no pending request %q", d.name, replyTo)
	}
	return t.DeliverDelta(replyTo, text)
}

// EndTurn implements gateway.TurnDriver. A turn the driver no longer knows is
// not an error: the request timed out and its record is gone.
func (d *Driver) EndTurn(replyTo string, err error) {
	if t := d.lookup(replyTo); t != nil {
		t.EndTurn(replyTo, err)
	}
}

// handleCatchAll answers anything else under the channel's path in OpenAI's
// error envelope, rather than httpd's plain-text 404.
func (d *Driver) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "invalid_request_error", "unknown_endpoint",
		fmt.Sprintf("%s %s is not an OpenAI-compatible endpoint on this channel (use chat/completions or models)", r.Method, r.URL.Path))
}

func (d *Driver) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "chat/completions accepts POST only")
		return
	}
	if d.opt.Ready != nil && !d.opt.Ready() {
		writeError(w, http.StatusServiceUnavailable, "server_error", "server_unavailable", "the runtime is draining; retry shortly")
		return
	}
	userUUID, from, ok := d.authenticate(w, r)
	if !ok {
		return
	}

	body, tooLarge, err := readBody(r, d.opt.MaxRequestBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request_error", "could not read the request body: "+err.Error())
		return
	}
	if tooLarge {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large",
			fmt.Sprintf("request body exceeds max_request_bytes (%d)", d.opt.MaxRequestBytes))
		return
	}
	req, agent, perr := d.parse(body, r.Header)
	if perr != nil {
		writeError(w, perr.status, perr.typ, perr.code, perr.message)
		return
	}

	// Idempotency: a retried request joins the turn that is still running; a
	// retry after that turn finished is a new turn. The derived message id is
	// what makes the two instances of one request the same message.
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	var idemHash string
	if idemKey != "" {
		sum := sha256.Sum256([]byte(idemKey))
		idemHash = hex.EncodeToString(sum[:16])
	}

	d.mu.Lock()
	t := d.idem[idemHash]
	joined := t != nil
	if t == nil {
		// A turn is always a fresh request id. The Idempotency-Key joins the
		// turn that is still running; a repeat after it finished is a new turn
		// (the plan's rule), and reusing the id would let it collide with the
		// old ephemeral session while that one is still exiting.
		req.ID = "req-" + uuid.NewString()
		d.opt.Requests.Add(req)
		t = newTurn(d, req)
		d.pending[req.ID] = t
		if idemHash != "" {
			d.idem[idemHash] = t
		}
	}
	d.mu.Unlock()

	if !joined {
		msg := d.messageFor(req, agent, from, userUUID, len(body))
		// A retried request is the same *message*: its id is derived from the
		// Idempotency-Key (a hash, never the raw header) so an audit trail or a
		// deduplicating consumer sees one message. The request/session id stays
		// fresh per turn, because a repeat after the turn finished is a new turn.
		if idemHash != "" {
			msg.ID = "msg-" + idemHash
		}
		d.sink.Submit(router.Inbound{
			Channel: d.name,
			Agent:   agent,
			Message: msg,
			// Delivery happens in the router goroutine. A refusal (ephemeral
			// cap, unknown agent) comes back here so the request can answer
			// 429/503 instead of waiting out its timeout.
			Fail: func(err error) { t.EndTurn(t.id, err) },
		})
	}

	if req.Stream {
		d.serveStream(w, r, t, req)
		return
	}
	d.serveBuffered(w, r, t, req)
}

func (d *Driver) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "models accepts GET only")
		return
	}
	if _, _, ok := d.authenticate(w, r); !ok {
		return
	}
	data := make([]map[string]any, 0, len(d.agents))
	for _, a := range d.agents {
		data = append(data, modelObject(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (d *Driver) handleModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "models/{id} accepts GET only")
		return
	}
	if _, _, ok := d.authenticate(w, r); !ok {
		return
	}
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, d.path), "models/")
	if !d.exposed[id] {
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			fmt.Sprintf("the model %q does not exist; this channel exposes %s", id, strings.Join(d.agents, ", ")))
		return
	}
	writeJSON(w, http.StatusOK, modelObject(id))
}

func modelObject(id string) map[string]any {
	return map[string]any{
		"id":       id,
		"object":   "model",
		"created":  0,
		"owned_by": "agentflow",
	}
}

// authenticate resolves the caller. user_token stamps the profile onto the
// message's provenance; key and none stamp known-empty, which is what keeps
// the sender-address fallback from minting a tenant out of a key label.
func (d *Driver) authenticate(w http.ResponseWriter, r *http.Request) (userUUID *string, from string, ok bool) {
	switch d.auth {
	case "user_token":
		token := bearerToken(r)
		if token == "" || d.opt.TokenUser == nil {
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "missing bearer token")
			return nil, "", false
		}
		user, found := d.opt.TokenUser(token)
		if !found {
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "the bearer token is unknown or revoked")
			return nil, "", false
		}
		u := user
		return &u, "user:" + d.name + ":" + user, true
	case "key":
		presented := bearerToken(r)
		if presented == "" {
			presented = r.Header.Get("x-api-key")
		}
		if presented == "" || !d.keyMatches(presented) {
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "the API key is missing or wrong")
			return nil, "", false
		}
		empty := ""
		return &empty, "user:" + d.name + ":key", true
	default: // none
		empty := ""
		return &empty, "user:" + d.name + ":anonymous", true
	}
}

// keyMatches compares in constant time against every configured key, so a
// wrong key's position does not leak through timing.
func (d *Driver) keyMatches(presented string) bool {
	got := []byte(presented)
	match := 0
	for _, k := range d.keys {
		match |= subtle.ConstantTimeCompare(got, k)
	}
	return match == 1
}

func bearerToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return ""
	}
	if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// readBody reads at most max+1 bytes so an oversized body is refused without
// buffering it all.
func readBody(r *http.Request, max int) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, int64(max)+1))
	_ = r.Body.Close()
	if err != nil {
		return nil, false, err
	}
	if len(b) > max {
		return nil, true, nil
	}
	return b, false, nil
}

// messageFor builds the small message the router and loop see. The body stays
// in the registry (D6): the payload carries metadata and a handle.
func (d *Driver) messageFor(req *chatreq.Request, agent, from string, userUUID *string, bytes int) session.Message {
	payload := map[string]any{
		"api":           "openai.chat",
		"request_id":    req.ID,
		"request_ref":   req.Ref,
		"model":         req.Model,
		"stream":        req.Stream,
		"message_count": len(req.Messages),
		"has_tools":     len(req.Tools) > 0,
		"bytes":         bytes,
	}
	if req.ConversationID != "" {
		payload["conversation_id"] = req.ConversationID
	}
	if req.User != "" {
		payload["user"] = req.User
	}
	if req.Metadata != nil {
		payload["metadata"] = req.Metadata
	}
	return session.Message{
		ID:      req.ID,
		Type:    "user",
		From:    from,
		Text:    lastUserText(req.Messages),
		Channel: d.name,
		ReplyTo: req.ID,
		Payload: payload,
		Provenance: &session.Provenance{
			Kind:      "channel",
			Principal: from,
			UserUUID:  userUUID,
		},
		Ts: time.Now().Unix(),
	}
}

// lastUserText is the message's text for safety ingress and display: the last
// user turn's text parts, or empty when the last turn is a tool result.
func lastUserText(msgs []session.ChatMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	last := msgs[len(msgs)-1]
	if last.Role == "tool" {
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		var b strings.Builder
		b.WriteString(msgs[i].Content)
		for _, p := range msgs[i].Parts {
			if p.Type == "text" && p.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// drop forgets a turn that never reached the agent.
func (d *Driver) drop(t *turn) {
	d.mu.Lock()
	delete(d.pending, t.id)
	for k, v := range d.idem {
		if v == t {
			delete(d.idem, k)
		}
	}
	d.mu.Unlock()
	d.opt.Requests.Remove(t.ref)
}

// remove forgets a finished turn, so a retry after it is a new turn.
func (d *Driver) remove(t *turn) {
	d.mu.Lock()
	delete(d.pending, t.id)
	for k, v := range d.idem {
		if v == t {
			delete(d.idem, k)
		}
	}
	d.mu.Unlock()
	t.forgetRequest()
}

// stopEphemeral terminates the one-turn session behind a turn whose client has
// gone. The key is the built-in route's (3.5); a custom route that keys
// differently makes this a no-op rather than a wrong kill.
func (d *Driver) stopEphemeral(t *turn) {
	if d.opt.StopSession == nil || t.agent == "" || t.conversationID != "" {
		return
	}
	d.opt.StopSession(t.agent + "|req:" + t.id)
}

func writeError(w http.ResponseWriter, status int, typ, code, message string) {
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, status, map[string]any{"error": errorEnvelope(typ, code, message)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// statusForError maps a turn failure to the HTTP contract. The loop's failures
// arrive as error strings; the two the table names by code are recognised by
// their markers, and everything else is an upstream error.
func statusForError(err error) (status int, typ, code, message string) {
	if err == nil {
		return http.StatusOK, "", "", ""
	}
	msg := err.Error()
	var timedOut *turnTimeout
	var unknownAgent *supervisor.UnknownAgentError
	switch {
	case errors.As(err, &timedOut):
		return http.StatusGatewayTimeout, "timeout", "timeout", msg
	case errors.As(err, &unknownAgent):
		return http.StatusNotFound, "invalid_request_error", "model_not_found", fmt.Sprintf("the model %q does not exist", unknownAgent.Agent)
	case strings.Contains(msg, "response_format has no equivalent"):
		return http.StatusBadRequest, "invalid_request_error", "invalid_request_error", msg
	case errors.Is(err, session.ErrSafetyBlocked) || strings.Contains(msg, "blocked by safety"):
		return http.StatusBadRequest, "invalid_request_error", "content_filter", msg
	case strings.Contains(msg, "user_quota_exhausted"):
		return http.StatusTooManyRequests, "insufficient_quota", "insufficient_quota", "the user's quota is exhausted"
	case errors.Is(err, supervisor.ErrTenantBusy), errors.Is(err, supervisor.ErrSessionCap):
		return http.StatusTooManyRequests, "rate_limit_exceeded", "rate_limit_exceeded", msg
	default:
		return http.StatusBadGateway, "upstream_error", "upstream_error", "the model call failed; the upstream error was logged"
	}
}
