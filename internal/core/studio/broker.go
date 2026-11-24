// Package studio backs the standalone Studio app: it observes sessions, runs
// playground turns through a synthetic channel, and keeps the bounded trace
// rings the app reads.
//
// It is deliberately engine-side and HTTP-free (the admin routes live in
// internal/webui). Nothing here changes execution: a missing observer, a full
// subscriber or a timed-out chat only affects what the app sees.
package studio

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentflow/internal/core/gateway"
	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
)

// ChannelName is the synthetic channel a playground turn replies on. It is
// registered on the gateway at boot, so an agent replies through the ordinary
// session egress path (safety, journaling and delivery all unchanged).
const ChannelName = "studio"

// Router is the part of the supervisor the broker needs.
type Router interface {
	// Deliver routes a message to (agent, key), spawning the session on first
	// contact.
	Deliver(agent, key string, msg session.Message) error
	// StopSession terminates one live session, if any.
	StopSession(sessionID string)
	// SessionBusy reports whether a session is live and currently processing a
	// message. Used to finish a chat whose loop exits instead of parking again.
	SessionBusy(sessionID string) (busy, live bool)
}

// Config tunes the broker's memory bounds.
type Config struct {
	// TraceEvents bounds each session's ring (default 2000).
	TraceEvents int
	// TraceSessions bounds how many session rings are retained (default 200).
	TraceSessions int
	// ChatTimeout is the default wait for one turn (default 120s).
	ChatTimeout time.Duration
	// ValidateUser, when set, must accept an act-as user uuid.
	ValidateUser func(uuid string) bool
}

// Broker keeps per-session trace rings and correlates playground replies.
type Broker struct {
	router   Router
	journal  runtime.Journal
	log      *slog.Logger
	cfg      Config
	maxEvent int
	maxSess  int
	timeout  time.Duration

	mu      sync.Mutex
	rings   map[string]*ring
	last    map[string]time.Time
	pending map[string]*pendingChat
}

type pendingChat struct {
	sessionKey string
	replies    chan Reply
	approvals  chan gateway.ApprovalPrompt
}

// Reply is one outbound message captured from a playground turn.
type Reply struct {
	Text        string       `json:"text"`
	Attachments []media.Part `json:"attachments,omitempty"`
	Status      string       `json:"status"`
}

// NewBroker builds a broker. A nil journal or logger is fine.
func NewBroker(router Router, journal runtime.Journal, cfg Config, log *slog.Logger) *Broker {
	if cfg.TraceEvents <= 0 {
		cfg.TraceEvents = 2000
	}
	if cfg.TraceSessions <= 0 {
		cfg.TraceSessions = 200
	}
	if cfg.ChatTimeout <= 0 {
		cfg.ChatTimeout = 120 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Broker{
		router:   router,
		journal:  journal,
		log:      log.With("module", "studio"),
		cfg:      cfg,
		maxEvent: cfg.TraceEvents,
		maxSess:  cfg.TraceSessions,
		timeout:  cfg.ChatTimeout,
		rings:    map[string]*ring{},
		last:     map[string]time.Time{},
		pending:  map[string]*pendingChat{},
	}
}

// Name implements gateway.Driver.
func (b *Broker) Name() string { return ChannelName }

// Deliver implements gateway.Driver: it hands a session reply to the chat
// waiting on that request id. A reply nobody is waiting for is dropped — a
// proactive push after a finished turn must not fail the loop's egress.
func (b *Broker) Deliver(replyTo, text string, attachments []media.Part) error {
	b.mu.Lock()
	p := b.pending[replyTo]
	b.mu.Unlock()
	if p == nil {
		return nil
	}
	select {
	case p.replies <- Reply{Text: text, Attachments: attachments, Status: "delivered"}:
	default: // the reader is gone; the trace still records the reply
	}
	return nil
}

// DeliverApproval implements gateway.ApprovalDriver: it pushes a redacted
// interactive approval prompt onto the active playground turn's SSE stream.
func (b *Broker) DeliverApproval(replyTo string, p gateway.ApprovalPrompt) error {
	b.mu.Lock()
	pc := b.pending[replyTo]
	b.mu.Unlock()
	if pc == nil {
		return nil
	}
	select {
	case pc.approvals <- p:
	default:
	}
	return nil
}

// ChatRequest is one playground turn.
type ChatRequest struct {
	Agent       string
	Thread      string // empty starts a new thread
	Text        string
	Attachments []media.Part
	ActAs       string // optional profile uuid to act as
	Timeout     time.Duration
}

// ChatResult summarizes a finished (or timed-out) turn.
type ChatResult struct {
	Thread   string  `json:"thread"`
	Session  string  `json:"session"`
	Replies  []Reply `json:"replies"`
	Events   int     `json:"events"`
	TimedOut bool    `json:"timed_out"`
}

// ChatEvent is what the HTTP handler streams.
type ChatEvent struct {
	Type     string                  `json:"type"` // trace | reply | approval | done | error
	Trace    *session.TraceEvent     `json:"trace,omitempty"`
	Reply    *Reply                  `json:"reply,omitempty"`
	Approval *gateway.ApprovalPrompt `json:"approval,omitempty"`
	Done     *ChatResult             `json:"done,omitempty"`
	Err      string                  `json:"error,omitempty"`
}

// ErrEmptyText is refused before a turn is delivered.
var ErrEmptyText = fmt.Errorf("message is empty")

// LoopError reports that the agent's loop failed while it was answering the
// turn. The session restarts on its own, but this turn is over: no turn.end
// will follow, so waiting for one would only run out the chat timeout.
type LoopError struct{ Reason string }

func (e *LoopError) Error() string { return "the loop failed: " + e.Reason }

// Check refuses a request that can never start a turn: an empty message, or
// an act-as id that is not a known profile. A streaming caller runs it before
// committing to a 200, so the refusal reaches the client as a status rather
// than as a stream that ends without a word. Chat runs it too.
func (b *Broker) Check(req ChatRequest) error {
	if strings.TrimSpace(req.Text) == "" && len(req.Attachments) == 0 {
		return ErrEmptyText
	}
	actAs := strings.TrimSpace(req.ActAs)
	if actAs != "" && b.cfg.ValidateUser != nil && !b.cfg.ValidateUser(actAs) {
		return fmt.Errorf("act-as user %s is not a known profile", actAs)
	}
	return nil
}

// Chat delivers one user message to an agent thread and streams the turn.
// emit is called on the calling goroutine; it must be quick (an SSE write).
//
// The stream ends in exactly one of two ways: a "done" event (the turn ended,
// timed out, or was refused by safety before it began), or an "error" event
// followed by a non-nil error (the message could not be delivered, or the
// loop failed mid-turn). A request refused by Check returns its error without
// emitting anything.
func (b *Broker) Chat(ctx context.Context, req ChatRequest, emit func(ChatEvent)) (ChatResult, error) {
	if err := b.Check(req); err != nil {
		return ChatResult{}, err
	}
	thread := req.Thread
	if thread == "" {
		thread = uuid.NewString()
	}
	actAs := strings.TrimSpace(req.ActAs)
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = b.timeout
	}
	metrics.Inc("agentflow_studio_chats")

	routeKey := ChannelName + ":" + thread
	sessionKey := req.Agent + "|" + routeKey
	// A chat watches only its own turn; earlier turns stay in the ring for
	// the trace view.
	ch, unsub := b.subscribeLive(sessionKey)
	defer unsub()

	requestID := uuid.NewString()
	pending := &pendingChat{
		sessionKey: sessionKey,
		replies:    make(chan Reply, 16),
		approvals:  make(chan gateway.ApprovalPrompt, 16),
	}
	b.mu.Lock()
	b.pending[requestID] = pending
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, requestID)
		b.mu.Unlock()
	}()

	prov := &session.Provenance{Kind: "studio", Principal: "studio:operator"}
	if actAs != "" {
		prov.UserUUID = &actAs
	}
	msg := session.Message{
		ID:          uuid.NewString(),
		Type:        "user",
		From:        "studio:operator",
		Text:        req.Text,
		Attachments: req.Attachments,
		Channel:     ChannelName,
		ReplyTo:     requestID,
		Payload:     map[string]any{"chat_id": thread, "studio": true, "text": req.Text},
		Provenance:  prov,
		Ts:          time.Now().Unix(),
	}
	b.journalInbound(ctx, req.Agent, thread, actAs, msg)

	result := ChatResult{Thread: thread, Session: sessionKey}
	if err := b.router.Deliver(req.Agent, routeKey, msg); err != nil {
		metrics.Inc("agentflow_studio_chat_errors")
		emit(ChatEvent{Type: "error", Err: err.Error()})
		return result, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	// Replies, approvals, and trace events travel on separate channels; drain
	// any buffered items that raced with the turn boundary before closing.
	drain := func() {
		for {
			select {
			case rep := <-pending.replies:
				result.Replies = append(result.Replies, rep)
				repCopy := rep
				emit(ChatEvent{Type: "reply", Reply: &repCopy})
			case ap := <-pending.approvals:
				apCopy := ap
				emit(ChatEvent{Type: "approval", Approval: &apCopy})
			default:
				return
			}
		}
	}
	finish := func() (ChatResult, error) {
		drain()
		emit(ChatEvent{Type: "done", Done: &result})
		return result, nil
	}

	sawTurn := false
	awaitingApproval := false
	// refused is set when safety ingress turned this message away. A
	// classifier block skips the turn outright (no turn.start, no turn.end),
	// so the only sign that it is over is the session going idle again.
	refused := false
	for {
		select {
		case <-ctx.Done():
			metrics.Inc("agentflow_studio_chat_errors")
			return result, ctx.Err()
		case ev := <-ch:
			result.Events++
			evCopy := ev
			emit(ChatEvent{Type: "trace", Trace: &evCopy})
			if ev.Turn == msg.ID && ev.Kind == "turn.start" {
				sawTurn = true
			}
			if ev.Turn == msg.ID && ev.Kind == "turn.end" {
				return finish()
			}
			if ev.Turn == msg.ID && ev.Kind == "safety" && ev.Op == "ingress" && !ev.OK {
				refused = true
			}
			// A loop failure carries no turn id, but a session answers one
			// message at a time: once this turn has started, the failure is
			// this turn's. Before that it belongs to an earlier message, and
			// ours is still waiting in the inbox for the restarted loop.
			if ev.Kind == "error" && sawTurn {
				metrics.Inc("agentflow_studio_chat_errors")
				drain()
				err := &LoopError{Reason: ev.Err}
				emit(ChatEvent{Type: "error", Err: err.Error()})
				return result, err
			}
		case ap := <-pending.approvals:
			awaitingApproval = true
			apCopy := ap
			emit(ChatEvent{Type: "approval", Approval: &apCopy})
		case rep := <-pending.replies:
			awaitingApproval = false
			result.Replies = append(result.Replies, rep)
			repCopy := rep
			emit(ChatEvent{Type: "reply", Reply: &repCopy})
		case <-tick.C:
			busy, live := b.router.SessionBusy(sessionKey)
			// The loop may exit instead of parking on the inbox (a one-shot
			// agent), in which case no turn.end arrives.
			if !live {
				return finish()
			}
			if sawTurn && !busy && !awaitingApproval && len(result.Replies) > 0 {
				return finish()
			}
			// Safety skipped the turn and the session is parked on its inbox
			// again. (A legacy drop still starts the turn, with the session
			// busy throughout, so it never lands here.)
			if refused && !sawTurn && !busy {
				return finish()
			}
		case <-timer.C:
			result.TimedOut = true
			metrics.Inc("agentflow_studio_chat_errors")
			return finish()
		}
	}
}

func (b *Broker) journalInbound(ctx context.Context, agent, thread, userUUID string, msg session.Message) {
	if b.journal == nil {
		return
	}
	prov := map[string]any{"kind": "studio", "principal": "studio:operator"}
	entry := runtime.JournalEntry{
		ID:         msg.ID,
		Ts:         msg.Ts,
		Direction:  "in",
		Status:     "accepted",
		Channel:    ChannelName,
		Chat:       thread,
		Sender:     msg.From,
		UserUUID:   userUUID,
		Agent:      agent,
		SessionID:  msg.Channel + ":" + thread,
		Type:       msg.Type,
		Text:       msg.Text,
		Provenance: prov,
	}
	if err := b.journal.RecordMessage(ctx, entry); err != nil {
		b.log.Warn("journal studio ingress write failed", "err", err)
	}
}

// Trace is the session.Tracer sink. It assigns the per-session sequence
// number, bounds the ring, and fans out without blocking the session.
func (b *Broker) Trace(ev session.TraceEvent) {
	key := ev.SessionID
	if key == "" {
		return
	}
	metrics.Inc("agentflow_studio_trace_events")
	ev.Thread = ThreadOf(key)

	b.mu.Lock()
	r, ok := b.rings[key]
	if !ok {
		r = newRing(b.maxEvent)
		b.rings[key] = r
	}
	b.last[key] = time.Now()
	b.evictLocked()
	b.mu.Unlock()

	r.push(ev)
}

// evictLocked drops the least-recently-active rings beyond the session bound.
// Caller holds b.mu.
func (b *Broker) evictLocked() {
	if len(b.rings) <= b.maxSess {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k := range b.rings {
		t := b.last[k]
		if oldestKey == "" || t.Before(oldest) {
			oldestKey, oldest = k, t
		}
	}
	if oldestKey != "" {
		delete(b.rings, oldestKey)
		delete(b.last, oldestKey)
	}
}

// SubscribeSession returns the live event stream for a session key, preceded
// by the buffered events already recorded, and an unsubscribe func.
func (b *Broker) SubscribeSession(sessionKey string) (<-chan session.TraceEvent, func()) {
	return b.ringFor(sessionKey).subscribeHistory()
}

func (b *Broker) subscribeLive(sessionKey string) (<-chan session.TraceEvent, func()) {
	return b.ringFor(sessionKey).subscribe()
}

// TraceSnapshot returns the buffered events for a studio thread.
func (b *Broker) TraceSnapshot(agent, thread string) []session.TraceEvent {
	return b.ringFor(agent + "|" + ChannelName + ":" + thread).snapshot()
}

// Thread is one observed studio session.
type Thread struct {
	Agent   string    `json:"agent"`
	Thread  string    `json:"thread"`
	Session string    `json:"session"`
	Last    time.Time `json:"last"`
	Events  int       `json:"events"`
}

// Threads lists observed studio threads, most recently active first.
func (b *Broker) Threads() []Thread {
	b.mu.Lock()
	type pair struct {
		key string
		at  time.Time
	}
	pairs := make([]pair, 0, len(b.rings))
	for k := range b.rings {
		if strings.Contains(k, "|"+ChannelName+":") {
			pairs = append(pairs, pair{k, b.last[k]})
		}
	}
	b.mu.Unlock()

	out := make([]Thread, 0, len(pairs))
	for _, p := range pairs {
		agent, _, _ := strings.Cut(p.key, "|")
		r := b.ringFor(p.key)
		out = append(out, Thread{
			Agent:   agent,
			Thread:  ThreadOf(p.key),
			Session: p.key,
			Last:    p.at,
			Events:  r.len(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

// StopThread terminates a studio thread's live session. The trace ring stays
// for inspection.
func (b *Broker) StopThread(agent, thread string) {
	b.router.StopSession(agent + "|" + ChannelName + ":" + thread)
}

func (b *Broker) ringFor(sessionKey string) *ring {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.rings[sessionKey]
	if !ok {
		r = newRing(b.maxEvent)
		b.rings[sessionKey] = r
	}
	if _, ok := b.last[sessionKey]; !ok {
		b.last[sessionKey] = time.Now()
	}
	return r
}

// ThreadOf extracts the studio thread from a session key (agent|studio:<id>),
// or "" for a session that is not a studio thread.
func ThreadOf(sessionKey string) string {
	_, key, ok := strings.Cut(sessionKey, "|")
	if !ok || !strings.HasPrefix(key, ChannelName+":") {
		return ""
	}
	return strings.TrimPrefix(key, ChannelName+":")
}

// ring is a bounded event log with non-blocking fan-out.
type ring struct {
	mu     sync.Mutex
	max    int
	seq    int64
	events []session.TraceEvent
	subs   map[chan session.TraceEvent]struct{}
}

func newRing(max int) *ring {
	if max <= 0 {
		max = 2000
	}
	return &ring{max: max, subs: map[chan session.TraceEvent]struct{}{}}
}

func (r *ring) push(ev session.TraceEvent) {
	r.mu.Lock()
	r.seq++
	ev.Seq = r.seq
	r.events = append(r.events, ev)
	if len(r.events) > r.max {
		r.events = r.events[len(r.events)-r.max:]
	}
	for ch := range r.subs {
		select {
		case ch <- ev:
		default: // slow watcher: drop, never block the session
		}
	}
	r.mu.Unlock()
}

func (r *ring) snapshot() []session.TraceEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]session.TraceEvent, len(r.events))
	copy(out, r.events)
	return out
}

func (r *ring) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *ring) subscribe() (<-chan session.TraceEvent, func()) {
	ch := make(chan session.TraceEvent, 256)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.subs, ch)
		close(ch)
		r.mu.Unlock()
	}
}

// subscribeHistory registers a subscriber and returns the buffered events
// under one lock, so a reconnect neither loses the gap nor duplicates it.
func (r *ring) subscribeHistory() (<-chan session.TraceEvent, func()) {
	ch := make(chan session.TraceEvent, 256)
	r.mu.Lock()
	hist := make([]session.TraceEvent, len(r.events))
	copy(hist, r.events)
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	for _, ev := range hist {
		select {
		case ch <- ev:
		default:
		}
	}
	return ch, func() {
		r.mu.Lock()
		delete(r.subs, ch)
		close(ch)
		r.mu.Unlock()
	}
}
