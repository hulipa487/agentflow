// Package session implements the session actor: one goroutine owning one
// Luau state, driven by a mailbox, executing op requests from Lua.
//
// Blocking ops (llm.*, channel sends) are dispatched to the shared worker
// pool so the actor goroutine stays responsive to reload and shutdown while
// an op is in flight.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"agentflow/internal/builtins"
	"agentflow/internal/core/address"
	"agentflow/internal/core/memory"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/safety"
	"agentflow/internal/vm"
)

// Provenance is assigned by the core at delivery time. Lua may inspect this
// metadata but cannot forge it through an op payload.
type Provenance struct {
	Kind      string `json:"kind"` // channel | agent | scheduler | system
	Principal string `json:"principal"`
	Parent    string `json:"parent,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// Identity is the runtime-owned authority attached to an actor's operations.
type Identity struct {
	SessionID    string
	Agent        string
	ParentID     string
	CanContact   map[string]bool
	Capabilities map[string]bool
}

// Message is the unified mailbox envelope (user/timer/agent/confirm/system).
type Message struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	From       string         `json:"from"`
	To         string         `json:"to,omitempty"`
	Text       string         `json:"text,omitempty"`
	Channel    string         `json:"channel,omitempty"`
	ReplyTo    string         `json:"reply_to,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	Provenance *Provenance    `json:"provenance,omitempty"`
	Ts         int64          `json:"ts,omitempty"`
}

// ChatMessage is one llm.chat turn crossing the bridge. Plain turns use
// Role+Content. An assistant turn that requested tools sets ToolCalls; a
// "tool" role turn carrying a result sets ToolCallID (+ ToolResult or Content).
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []ToolCallSpec `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolResult any            `json:"tool_result,omitempty"`
}

// ToolCallSpec is one tool invocation crossing the bridge (name + parsed args).
type ToolCallSpec struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// ToolSpec is a provider-agnostic tool definition a loop passes via llm.chat
// opts. It crosses the bridge as {name, description, parameters} and is
// reshaped per provider at request time.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// Op is a request yielded by Lua through the async bridge.
type Op struct {
	Type        string        `json:"type"`
	Text        string        `json:"text,omitempty"`
	Level       string        `json:"level,omitempty"`
	Msg         string        `json:"msg,omitempty"`
	Seconds     float64       `json:"seconds,omitempty"`
	Messages    []ChatMessage `json:"messages,omitempty"`
	Model       string        `json:"model,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stream      string        `json:"stream,omitempty"`
	Tools       []ToolSpec    `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`

	// Embedding / rerank ops (llm.embed, llm.rerank).
	Inputs    []string `json:"inputs,omitempty"`    // embed: texts to embed
	Documents []string `json:"documents,omitempty"` // rerank: candidate documents
	TopN      int      `json:"top_n,omitempty"`     // rerank: keep at most this many

	// Proactive channel egress (session.push).
	Channel string `json:"channel,omitempty"`
	ReplyTo string `json:"reply_to,omitempty"`

	// Store / memory / tool ops.
	Table  string         `json:"table,omitempty"`
	Key    string         `json:"key,omitempty"`
	Value  any            `json:"value,omitempty"`
	TTL    float64        `json:"ttl,omitempty"`
	Vector []float32      `json:"vector,omitempty"`
	Query  memory.Query   `json:"query,omitempty"`
	Tool   string         `json:"tool,omitempty"`
	Args   map[string]any `json:"args,omitempty"`

	// HTTP op (http.request).
	Method  string            `json:"method,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Json    map[string]any    `json:"json,omitempty"`
	// QueryParams is the http.request ?key=value table.
	QueryParams map[string]string `json:"query_params,omitempty"`
	// EnvName is the os.env op's variable name.
	EnvName string `json:"name,omitempty"`
	// Auth names a stored credential to resolve at request time (never the
	// secret itself). The loop references it; Go injects the resolved value.
	Auth *CredentialRef `json:"auth,omitempty"`

	// Mail ops (mail.imap.fetch, mail.smtp.send).
	Mailbox  string   `json:"mailbox,omitempty"`   // IMAP: folder to fetch (default INBOX)
	Limit    int      `json:"limit,omitempty"`     // IMAP: max messages to return (default 10)
	Unseen   bool     `json:"unseen,omitempty"`    // IMAP: fetch only unread
	MailFrom string   `json:"mail_from,omitempty"` // SMTP: From address
	MailTo   []string `json:"mail_to,omitempty"`   // SMTP: recipients
	Subject  string   `json:"subject,omitempty"`   // SMTP: subject
	TextBody string   `json:"text_body,omitempty"` // SMTP: plain body
	MailHost string   `json:"mail_host,omitempty"` // SMTP/IMAP: host
	MailPort int      `json:"mail_port,omitempty"` // SMTP/IMAP: port
	MailUser string   `json:"mail_user,omitempty"` // SMTP/IMAP: username (cred resolves password)

	// Multi-agent operations.
	Address string         `json:"address,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
	Timeout float64        `json:"timeout,omitempty"`
	Profile string         `json:"profile,omitempty"`
	Spec    map[string]any `json:"spec,omitempty"`
	Request string         `json:"request,omitempty"`

	// Scheduler operations.
	Interval float64 `json:"interval,omitempty"`
	Delay    float64 `json:"delay,omitempty"`
	Cron     string  `json:"cron,omitempty"`
	TimerID  string  `json:"timer_id,omitempty"`

	// Shell ops.
	ShellHandle   string            `json:"shell_handle,omitempty"`
	Cmd           string            `json:"cmd,omitempty"`
	Path          string            `json:"path,omitempty"`
	Content       string            `json:"content,omitempty"`
	Image         string            `json:"image,omitempty"`
	ShellProvider string            `json:"provider,omitempty"`
	WorkDir       string            `json:"workdir,omitempty"`
	Net           string            `json:"network,omitempty"`
	MemLimit      string            `json:"mem_limit,omitempty"`
	CPULimit      float64           `json:"cpu_limit,omitempty"`
	ShellEnv      map[string]string `json:"env,omitempty"`
	ShellOpts     map[string]any    `json:"shell_opts,omitempty"`
	Host          string            `json:"host,omitempty"`
	User          string            `json:"user,omitempty"`
	Password      string            `json:"password,omitempty"`
	KeyFile       string            `json:"key_file,omitempty"`

	// Confirm bypass.
	Confirmed bool   `json:"confirmed,omitempty"`
	ConfirmID string `json:"confirm_id,omitempty"`
}

// CredentialRef names a stored credential to resolve at op-handler time. It
// carries only the service name — never the secret — so a credential value
// never crosses the Lua bridge. The resolver maps it to a per-user secret.
type CredentialRef struct {
	Service string `json:"service"`
}

// SchedulerService is implemented by the scheduler package and injected into
// the actor. The actor dispatches scheduler.* ops to it.
type SchedulerService interface {
	Every(owner string, intervalSec float64) (string, error)
	After(owner string, delaySec float64) (string, error)
	Cron(owner string, expr string) (string, error)
	Cancel(owner string, timerID string) error
}

// Gateway is the egress half of the channel registry.
type Gateway interface {
	Send(channel, replyTo, text string) error
}

// UserResolver resolves a user UUID to a delivery target for proactive push
// (session.push_user). It is satisfied by the identity.Registry; defined
// here (not imported from identity) to avoid a session↔identity import cycle.
type UserResolver interface {
	LookupUser(uuid string) (channel, replyTo string, ok bool)
}

// AgentSummary is safe runtime metadata returned by agent.list.
type AgentSummary struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	ParentID  string `json:"parent_id,omitempty"`
}

// AgentService is implemented by the supervisor. It is deliberately defined
// in the session package so actors can invoke it without importing supervisor
// and creating a package cycle.
type AgentService interface {
	Send(ctx context.Context, source Identity, address string, payload map[string]any) error
	Request(ctx context.Context, source Identity, address string, payload map[string]any, timeout time.Duration) (Message, error)
	Reply(ctx context.Context, source Identity, requestID string, payload map[string]any) error
	Spawn(ctx context.Context, source Identity, profile string, spec map[string]any) (SpawnResult, error)
	List(source Identity) []AgentSummary
}

// SpawnResult mirrors the supervisor's spawn result for the Lua surface.
type SpawnResult struct {
	Address   string `json:"address"`
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
}

// OpHandler executes one op type and returns (response JSON, ok).
// Handlers for driver-backed ops (llm.*) are injected at spawn.
type OpHandler func(ctx context.Context, op Op) (string, bool)

// Info is per-agent data shared by all of the agent's sessions.
type Info struct {
	Name          string
	Model         string
	Instructions  *StringBox // hot-reloadable without a session restart
	HistoryBudget int
	Memory        *memory.AgentMemory
	Shell         map[string]any
	Skills        []string
	Capabilities  []string
}

// StringBox is a race-free string cell (this toolchain's sync/atomic lacks
// the typed String cell).
type StringBox struct{ v atomic.Value }

func (b *StringBox) Load() string {
	s, _ := b.v.Load().(string)
	return s
}

func (b *StringBox) Store(s string) { b.v.Store(s) }

// blockingOps run on the worker pool; everything else is inline.
var blockingOps = map[string]bool{
	"send":              true,
	"session.push":      true,
	"session.push_user": true,
	"llm.chat":          true,
	"llm.embed":         true,
	"llm.rerank":        true,
	"llm.stream.open":   true,
	"llm.stream.next":   true,
	"tools.run":         true,
	"store.put":         true,
	"store.get":         true,
	"store.query":       true,
	"store.delete":      true,
	"shell.spawn":       true,
	"shell.exec":        true,
	"shell.write":       true,
	"shell.destroy":     true,
	"http.request":      true,
	"mail.imap.fetch":   true,
	"mail.smtp.send":    true,
	"agent.send":        true,
	"agent.request":     true,
	"agent.reply":       true,
	"agent.spawn":       true,
	"agent.list":        true,
	"scheduler.every":   true,
	"scheduler.after":   true,
	"scheduler.cron":    true,
	"scheduler.cancel":  true,
}

// EndReason identifies why an actor left the supervisor.
type EndReason string

const (
	EndShutdown   EndReason = "shutdown"
	EndTerminated EndReason = "terminated"
)

// Actor is a single agent session.
type Actor struct {
	Name        string // session key (agent|route-key), for logs
	Identity    Identity
	Info        *Info
	OnExit      func(Identity, EndReason)
	LoopFile    string   // re-read on restart; empty for builtins
	LoopSrc     string   // source used when LoopFile is empty
	SupportSrc  string   // extra chunk loaded before the plugin (builtin:token_budget)
	SupportSrcs []string // support chunks loaded before the loop plugin
	InstrBudget int64

	Mailbox chan Message

	gw       Gateway
	agents   AgentService
	sched    SchedulerService
	users    UserResolver
	safety   *safety.Dispatcher
	handlers map[string]OpHandler
	pool     *pool.Pool

	reload chan struct{}
	log    *slog.Logger

	// exiting is set by the session.exit op (actor goroutine only). Run
	// checks it after runOnce returns so an exited session is not restarted.
	exiting bool

	// busy reports whether the actor is actively processing a message (vs.
	// blocked waiting for the next inbox). Read by the supervisor's status
	// snapshot for the TUI; written only by the actor goroutine.
	busy atomic.Bool
}

func New(name string, identity Identity, info *Info, gw Gateway, agents AgentService, sched SchedulerService, users UserResolver, safe *safety.Dispatcher, handlers map[string]OpHandler, p *pool.Pool, log *slog.Logger) *Actor {
	return &Actor{
		Name:        name,
		Identity:    identity,
		Info:        info,
		InstrBudget: 5_000_000,
		Mailbox:     make(chan Message, 64),
		gw:          gw,
		agents:      agents,
		sched:       sched,
		users:       users,
		safety:      safe,
		handlers:    handlers,
		pool:        p,
		reload:      make(chan struct{}, 1),
		log:         log.With("session", name),
	}
}

// Reload requests a fresh-state restart at the next safe point.
func (a *Actor) Reload() {
	select {
	case a.reload <- struct{}{}:
	default: // already pending
	}
}

// Busy reports whether the actor is actively processing a message rather than
// blocked waiting for the next inbox. Safe for concurrent reads (TUI snapshot).
func (a *Actor) Busy() bool { return a.busy.Load() }

// Run is the actor's main loop. It owns the Luau state; call from one
// goroutine only. It restarts the loop on crashes and reload signals until
// ctx is done.
func (a *Actor) Run(ctx context.Context) {
	defer func() {
		if a.OnExit == nil {
			return
		}
		reason := EndTerminated
		if ctx.Err() != nil {
			reason = EndShutdown
		}
		a.OnExit(a.Identity, reason)
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		crashed := a.runOnce(ctx)
		if a.exiting {
			a.log.Info("session exited via session.exit")
			return
		}
		if crashed {
			a.log.Warn("loop crashed; restarting in 1s")
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

func (a *Actor) loadSource() (string, error) {
	if a.LoopFile != "" {
		// LoopFile may be a directory (a multi-module loop); builtins.Resolve
		// handles both file and directory, concatenating a directory's *.lua
		// members in sorted order. For a plain file it reads the file.
		src, _, err := builtins.Resolve(a.LoopFile)
		return src, err
	}
	return a.LoopSrc, nil
}

func (a *Actor) runOnce(ctx context.Context) (crashed bool) {
	src, err := a.loadSource()
	if err != nil {
		a.log.Error("cannot read loop plugin", "err", err)
		return true
	}

	st := vm.New(a.InstrBudget)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		a.log.Error("prelude load failed", "err", err)
		return true
	}
	if a.SupportSrc != "" {
		if err := st.Eval("@builtin:token_budget", a.SupportSrc); err != nil {
			a.log.Error("support chunk load failed", "err", err)
			return true
		}
	}
	for i, chunk := range a.SupportSrcs {
		if err := st.Eval(fmt.Sprintf("@builtin:support:%d", i), chunk); err != nil {
			a.log.Error("support chunk load failed", "idx", i, "err", err)
			return true
		}
	}

	status, msg := st.Start("loop", src)
	a.log.Info("loop started")
	var current Message // inbound message being processed; replies go here

	for {
		switch status {
		case vm.Finished:
			a.log.Info("loop finished")
			return false
		case vm.Failed:
			a.log.Error("loop error", "err", msg)
			return true
		}

		var op Op
		if err := json.Unmarshal([]byte(msg), &op); err != nil {
			a.log.Error("bad op from lua", "err", err, "raw", msg)
			return true
		}

		var resp string
		var ok, proceed bool

		if blockingOps[op.Type] {
			resp, ok, proceed = a.dispatchBlocking(ctx, op, &current)
		} else {
			resp, ok, proceed = a.dispatchInline(ctx, op, &current)
		}
		if !proceed {
			return false // shutdown or reload
		}

		// Confirm interception: when tools.run returns needs_confirm, park the
		// coroutine, send a confirm prompt, block for the user's confirm reply,
		// then re-dispatch with Confirmed=true. The Lua coroutine stays suspended
		// throughout — it yields once, resumes once.
		if op.Type == "tools.run" && ok && isConfirmRequest(resp) {
			resp, ok, proceed = a.doConfirm(ctx, op, &current, resp)
			if !proceed {
				return false
			}
		}

		status, msg = st.Resume(resp, ok)
	}
}

// dispatchInline runs fast ops on the actor goroutine.
func (a *Actor) dispatchInline(ctx context.Context, op Op, current *Message) (resp string, ok, proceed bool) {
	switch op.Type {
	case "inbox":
		m, goOn := a.waitInbox(ctx)
		if !goOn {
			return "", false, false
		}
		// Safety ingress: run the message through the dispatcher before the
		// loop sees it. A drop means the loop gets a no-op message.
		if a.safety != nil {
			res := a.safety.Ingress(ctx, safety.IngressInput{
				Message: m.Text,
				Source:  m.Type,
			})
			if res.Drop {
				a.log.Info("safety ingress dropped message", "reason", res.Reason)
				m.Text = "" // loop sees an empty message, not the original
			} else if res.Text != m.Text {
				m.Text = res.Text
			}
		}
		*current = m
		r, _ := jsonString(m)
		return r, true, true

	case "sleep":
		if !sleepOr(ctx, a.reload, time.Duration(op.Seconds*float64(time.Second))) {
			return "", false, false
		}
		return "true", true, true

	case "log":
		a.log.Log(ctx, levelOf(op.Level), op.Msg)
		return "true", true, true

	case "session.exit":
		// The coroutine is never resumed; Run sees the flag and lets the
		// actor terminate instead of restarting the loop.
		a.exiting = true
		return "true", true, false

	case "agent.info":
		return a.infoJSON(), true, true

	default:
		// Stamp the tenant user UUID for inline handlers, mirroring execBlocking.
		ctx = WithUserUUID(ctx, userFromMessage(current))
		if h, found := a.handlers[op.Type]; found {
			r, ok := h(ctx, op)
			return r, ok, true
		}
		return fmt.Sprintf("%q", "unknown op "+op.Type), false, true
	}
}

// dispatchBlocking runs an op on the worker pool and waits for the result,
// a reload signal, or shutdown. On abandon the op finishes in the background
// and its result is discarded (the VM is dead by then anyway).
func (a *Actor) dispatchBlocking(ctx context.Context, op Op, current *Message) (resp string, ok, proceed bool) {
	type result struct {
		resp string
		ok   bool
	}
	resCh := pool.Call(a.pool, func() result {
		r, ok := a.execBlocking(ctx, op, current)
		return result{r, ok}
	})
	select {
	case res := <-resCh:
		return res.resp, res.ok, true
	case <-a.reload:
		a.log.Info("hot reload: restarting loop")
		return "", false, false
	case <-ctx.Done():
		return "", false, false
	}
}

func (a *Actor) execBlocking(ctx context.Context, op Op, current *Message) (string, bool) {
	ctx = WithOwner(ctx, a.Name)
	ctx = WithUserUUID(ctx, userFromMessage(current))
	if op.Type == "send" {
		if current.Channel == "" {
			return `"no active message to reply to"`, false
		}
		return a.egress(ctx, current.Channel, current.ReplyTo, op.Text)
	}
	if op.Type == "session.push" {
		// Proactive egress: send to an explicit channel/recipient without an
		// active inbound message. Capability-gated; same safety egress as send.
		if !a.Identity.Capabilities["channel.push"] {
			return `"agent lacks channel.push capability"`, false
		}
		if op.Channel == "" {
			return `"session.push requires a channel"`, false
		}
		return a.egress(ctx, op.Channel, op.ReplyTo, op.Text)
	}
	if op.Type == "session.push_user" {
		// Proactive egress to a user by identity UUID. Channel-agnostic: the
		// loop only knows the UUID (from msg.from of a prior turn); the
		// resolver maps it back to {channel, reply_to}. Same capability gate
		// and safety egress as session.push.
		if !a.Identity.Capabilities["channel.push"] {
			return `"agent lacks channel.push capability"`, false
		}
		if a.users == nil {
			return `"identity not enabled on this runtime"`, false
		}
		addr, err := address.Parse(op.Address)
		if err != nil || addr.Kind != address.User {
			return `"session.push_user requires a user:<uuid> address"`, false
		}
		channel, replyTo, ok := a.users.LookupUser(addr.User)
		if !ok {
			return `"unknown user"`, false
		}
		return a.egress(ctx, channel, replyTo, op.Text)
	}
	if a.agents != nil {
		switch op.Type {
		case "agent.send":
			if err := a.agents.Send(ctx, a.Identity, op.Address, op.Payload); err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			return "true", true
		case "agent.request":
			msg, err := a.agents.Request(ctx, a.Identity, op.Address, op.Payload, time.Duration(op.Timeout*float64(time.Second)))
			if err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			r, _ := jsonString(msg)
			return r, true
		case "agent.reply":
			if err := a.agents.Reply(ctx, a.Identity, op.Request, op.Payload); err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			return "true", true
		case "agent.list":
			r, _ := jsonString(a.agents.List(a.Identity))
			return r, true
		case "agent.spawn":
			res, err := a.agents.Spawn(ctx, a.Identity, op.Profile, op.Spec)
			if err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			r, _ := jsonString(res)
			return r, true
		}
	}
	if a.sched != nil {
		switch op.Type {
		case "scheduler.every":
			id, err := a.sched.Every(a.Name, op.Interval)
			if err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			r, _ := jsonString(map[string]any{"id": id})
			return r, true
		case "scheduler.after":
			id, err := a.sched.After(a.Name, op.Delay)
			if err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			r, _ := jsonString(map[string]any{"id": id})
			return r, true
		case "scheduler.cron":
			id, err := a.sched.Cron(a.Name, op.Cron)
			if err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			r, _ := jsonString(map[string]any{"id": id})
			return r, true
		case "scheduler.cancel":
			if err := a.sched.Cancel(a.Name, op.TimerID); err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			return "true", true
		}
	}
	if h, found := a.handlers[op.Type]; found {
		return h(ctx, op)
	}
	r, _ := jsonString("unknown op " + op.Type)
	return r, false
}

// egress runs text through the safety dispatcher and delivers it to a
// channel recipient via the gateway. A safety drop means nothing is sent.
func (a *Actor) egress(ctx context.Context, channel, replyTo, text string) (string, bool) {
	if a.safety != nil {
		res := a.safety.Egress(ctx, safety.EgressInput{
			Text:   text,
			Source: "agent",
		})
		if res.Drop {
			a.log.Info("safety egress dropped reply", "reason", res.Reason)
			return `"reply blocked by safety filter"`, false
		}
		text = res.Text
	}
	if a.gw != nil {
		if err := a.gw.Send(channel, replyTo, text); err != nil {
			a.log.Warn("egress failed", "err", err)
			r, _ := jsonString(err.Error())
			return r, false
		}
	}
	return "true", true
}

func (a *Actor) infoJSON() string {
	instructions := ""
	if a.Info.Instructions != nil {
		instructions = a.Info.Instructions.Load()
	}
	budget := a.Info.HistoryBudget
	if budget <= 0 {
		budget = 6000
	}
	mem := map[string]any{}
	if a.Info.Memory != nil {
		stores := map[string]any{}
		for name, bind := range a.Info.Memory.Stores {
			stores[name] = map[string]any{"backend": bind.Backend, "table": bind.Table, "features": bind.Features}
		}
		mem["stores"] = stores
		mem["write"] = a.Info.Memory.Write
		mem["recall"] = a.Info.Memory.Recall
		mem["embed_model"] = a.Info.Memory.EmbedModel
		mem["rerank_model"] = a.Info.Memory.RerankModel
		mem["oversample"] = a.Info.Memory.Oversample
	}
	r, _ := jsonString(map[string]any{
		"name":           a.Info.Name,
		"session_id":     a.Identity.SessionID,
		"address":        "session:" + a.Identity.SessionID,
		"model":          a.Info.Model,
		"instructions":   instructions,
		"history_budget": budget,
		"memory":         mem,
		"shell":          a.Info.Shell,
		"skills":         a.Info.Skills,
		"capabilities":   a.Info.Capabilities,
	})
	return r
}

// waitInbox blocks for the next message, a reload signal, or shutdown.
func (a *Actor) waitInbox(ctx context.Context) (Message, bool) {
	a.busy.Store(false) // idle while parked on the mailbox
	select {
	case m := <-a.Mailbox:
		a.busy.Store(true)
		return m, true
	case <-a.reload:
		a.log.Info("hot reload: restarting loop")
		return Message{}, false
	case <-ctx.Done():
		return Message{}, false
	}
}

func sleepOr(ctx context.Context, reload chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-reload:
		return false
	case <-ctx.Done():
		return false
	}
}

func levelOf(l string) slog.Level {
	switch l {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func jsonString(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

type ownerKeyType struct{}

var ownerKey ownerKeyType

// WithOwner returns a context carrying the session key. Shell and confirm
// handlers extract this to enforce per-session ownership.
func WithOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey, owner)
}

// OwnerFromCtx extracts the session key injected by WithOwner.
func OwnerFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v := ctx.Value(ownerKey)
	s, _ := v.(string)
	return s
}

type userKeyType struct{}

var userKey userKeyType

// WithUserUUID returns a context carrying the current tenant's user UUID.
// It is stamped by the actor from the core-owned inbound message (never from
// Lua), so handlers can resolve per-tenant credentials without trusting the
// loop. Empty when no inbound user is in scope (e.g. proactive scheduler ops).
func WithUserUUID(ctx context.Context, userUUID string) context.Context {
	return context.WithValue(ctx, userKey, userUUID)
}

// UserUUIDFromCtx extracts the user UUID injected by WithUserUUID.
func UserUUIDFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v := ctx.Value(userKey)
	s, _ := v.(string)
	return s
}

// userFromMessage recovers the user UUID from an inbound message: prefer the
// identity-stashed payload field, else strip "user:" from From.
func userFromMessage(m *Message) string {
	if m == nil {
		return ""
	}
	if p, ok := m.Payload["user_uuid"].(string); ok && p != "" {
		return p
	}
	if from := m.From; from != "" {
		if u := strings.TrimPrefix(from, "user:"); u != from && u != "" {
			return u
		}
	}
	return ""
}

// isConfirmRequest checks whether a response JSON encodes a needs_confirm=true result.
func isConfirmRequest(respJSON string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(respJSON), &m); err != nil {
		return false
	}
	v, ok := m["needs_confirm"]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// doConfirm sends a confirm prompt to the user, blocks for a confirm reply,
// then re-dispatches the tools.run op with Confirmed=true if approved.
func (a *Actor) doConfirm(ctx context.Context, op Op, current *Message, respJSON string) (resp string, ok, proceed bool) {
	var ci struct {
		ConfirmID string `json:"confirm_id"`
		Tool      string `json:"tool"`
	}
	if err := json.Unmarshal([]byte(respJSON), &ci); err != nil {
		return `"confirm parse error"`, false, true
	}

	prompt := fmt.Sprintf(
		"⚠️ Confirm: run tool %q? Reply YES to proceed or NO to cancel [ref:%s]",
		ci.Tool, ci.ConfirmID,
	)
	if a.gw != nil && current != nil && current.Channel != "" {
		if err := a.gw.Send(current.Channel, current.ReplyTo, prompt); err != nil {
			a.log.Warn("confirm prompt send failed", "err", err)
		}
	}
	a.log.Info("confirm prompt sent", "tool", ci.Tool, "confirm_id", ci.ConfirmID)

	var confirmed bool
	for {
		msg, goOn := a.waitInbox(ctx)
		if !goOn {
			return "", false, false
		}
		if msg.Text == "" {
			continue
		}
		ref := fmt.Sprintf("[ref:%s]", ci.ConfirmID)
		if !strings.Contains(msg.Text, ref) {
			continue
		}
		upper := strings.ToUpper(msg.Text)
		confirmed = strings.Contains(upper, "YES")
		break
	}

	if !confirmed {
		r, _ := jsonString("tool invocation denied by user")
		a.log.Info("confirm denied", "tool", ci.Tool, "confirm_id", ci.ConfirmID)
		return r, false, true
	}

	a.log.Info("confirm approved; re-dispatching", "tool", ci.Tool)
	op.Confirmed = true
	op.ConfirmID = ci.ConfirmID

	type result struct {
		resp string
		ok   bool
	}
	resCh := pool.Call(a.pool, func() result {
		r, o := a.execBlocking(ctx, op, current)
		return result{r, o}
	})
	select {
	case res := <-resCh:
		return res.resp, res.ok, true
	case <-a.reload:
		return "", false, false
	case <-ctx.Done():
		return "", false, false
	}
}
