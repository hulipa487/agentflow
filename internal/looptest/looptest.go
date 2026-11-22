// Package looptest runs a Lua loop against a scenario without a model, a
// network, a store, or a running engine. It is the offline half of
// `agentflow loop test`: the actor is real, the loop is real, and the
// dependencies (LLM, tools, scheduler, channels) are deterministic fakes.
//
// A scenario is deliberately data, not Go: the same file works in a local
// checkout and in CI.
package looptest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"agentflow/internal/core/gateway"
	"agentflow/internal/core/media"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/safety"
	"agentflow/internal/core/session"
	"agentflow/internal/luacode"
)

// Scenario is the YAML shape. Loop or LoopSource is required.
type Scenario struct {
	Agent        string            `yaml:"agent"`
	Loop         string            `yaml:"loop"`
	LoopSource   string            `yaml:"loop_source,omitempty"`
	Instructions string            `yaml:"instructions"`
	Model        string            `yaml:"model"`
	Safety       string            `yaml:"safety,omitempty"` // ""|"none" | "default"
	Packages     map[string]string `yaml:"packages"`
	Timeout      string            `yaml:"timeout"`
	Messages     []Input           `yaml:"messages"`
	LLM          []LLMReply        `yaml:"llm"`
	Tools        map[string]any    `yaml:"tools"`
	// Ops maps an op type to a canned JSON response for ops the harness does
	// not model (http.request, store.*, files.*, ...).
	Ops map[string]string `yaml:"ops"`
	// Expect is the exact ordered list of sends the loop must produce.
	Expect []string `yaml:"expect"`
	// ExpectContains is an ordered list of substrings; each must appear in
	// some send after the previous match.
	ExpectContains []string `yaml:"expect_contains"`
	// ExpectOps is the ordered list of op types the loop must invoke.
	ExpectOps []string `yaml:"expect_ops"`

	// baseDir is the scenario file's directory; relative loop and package
	// paths resolve against it, so a scenario works from any CWD.
	baseDir string
}

type Input struct {
	From string `yaml:"from"`
	Text string `yaml:"text"`
}

type LLMUsage struct {
	Input     int `yaml:"input" json:"input,omitempty"`
	Output    int `yaml:"output" json:"output,omitempty"`
	Reasoning int `yaml:"reasoning" json:"reasoning,omitempty"`
}

type LLMReply struct {
	Match     string           `yaml:"match"`
	Text      string           `yaml:"text"`
	ToolCalls []map[string]any `yaml:"tool_calls"`
	Usage     *LLMUsage        `yaml:"usage,omitempty"`
}

// AssertionResult captures one evaluated scenario expectation.
type AssertionResult struct {
	Kind     string `json:"kind"` // expect | expect_contains | expect_ops | runtime
	Index    int    `json:"index"`
	Expected string `json:"expected"`
	Actual   string `json:"actual,omitempty"`
	OK       bool   `json:"ok"`
	Err      string `json:"err,omitempty"`
}

// Result holds the structured outcome, outputs, assertion checks, and full
// execution trace of a scenario run.
type Result struct {
	OK         bool                 `json:"ok"`
	Error      string               `json:"error,omitempty"`
	Agent      string               `json:"agent,omitempty"`
	Loop       string               `json:"loop,omitempty"`
	Model      string               `json:"model,omitempty"`
	DurationMS int64                `json:"duration_ms"`
	Sends      []string             `json:"sends"`
	Ops        []string             `json:"ops"`
	Assertions []AssertionResult    `json:"assertions"`
	Events     []session.TraceEvent `json:"events"`
}

var pkgMu sync.Mutex

// Run executes a scenario and returns a readable failure. It never panics on a
// loop error: the actor logs it and the expectation check reports what the
// loop actually produced.
func Run(sc *Scenario, out io.Writer) error {
	res, err := RunWithResult(context.Background(), sc)
	if err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("%s", res.Error)
	}
	if out != nil {
		fmt.Fprintf(out, "ok: %s (%d sends, %d ops)\n", first(sc.Loop, sc.Agent), len(res.Sends), len(res.Ops))
		for _, s := range res.Sends {
			fmt.Fprintf(out, "  -> %s\n", s)
		}
	}
	return nil
}

// RunWithResult executes a scenario and returns structured sends, ops,
// assertion outcomes, and captured session trace events.
func RunWithResult(ctx context.Context, sc *Scenario) (*Result, error) {
	if sc == nil {
		return nil, fmt.Errorf("scenario is nil")
	}
	if strings.TrimSpace(sc.Loop) == "" && strings.TrimSpace(sc.LoopSource) == "" {
		return nil, fmt.Errorf("scenario: loop or loop_source is required")
	}
	msgs := append([]Input(nil), sc.Messages...)
	if len(msgs) == 0 {
		msgs = []Input{{From: "tester", Text: "hello"}}
	}
	timeout := 3 * time.Second
	if sc.Timeout != "" {
		d, err := time.ParseDuration(sc.Timeout)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("scenario: timeout %q is not a positive duration", sc.Timeout)
		}
		timeout = d
	}

	var safeProfile *safety.Profile
	switch strings.ToLower(strings.TrimSpace(sc.Safety)) {
	case "", "none":
		safeProfile = safety.None
	case "default":
		safeProfile = safety.DefaultProfile()
	default:
		return nil, fmt.Errorf("scenario: unknown safety profile %q (want none or default)", sc.Safety)
	}

	if sc.Packages != nil {
		pkgMu.Lock()
		prev := luacode.Packages()
		defer func() {
			luacode.SetPackages(prev)
			pkgMu.Unlock()
		}()
		resolved := make(map[string]string, len(sc.Packages))
		for name, dir := range sc.Packages {
			resolved[name] = resolve(dir, sc.baseDir)
		}
		luacode.SetPackages(resolved)
	}

	var (
		src   string
		watch string
		err   error
	)
	if strings.TrimSpace(sc.LoopSource) != "" {
		name := first(sc.Loop, "inline.lua")
		src, err = luacode.ResolveSource(name, sc.LoopSource)
		if err != nil {
			return nil, fmt.Errorf("loop: %w", err)
		}
	} else {
		loopRef := resolve(sc.Loop, sc.baseDir)
		src, watch, err = luacode.Resolve(loopRef)
		if err != nil {
			return nil, fmt.Errorf("loop: %w", err)
		}
	}
	support, err := luacode.SupportChunks()
	if err != nil {
		return nil, fmt.Errorf("support chunks: %w", err)
	}

	started := time.Now()
	rec := &recorder{}
	tc := &traceCollector{}
	handlers, ops := scenarioHandlers(sc, rec)
	info := &session.Info{
		Name:         first(sc.Agent, "scenario"),
		Model:        first(sc.Model, "test-model"),
		Instructions: &session.StringBox{},
	}
	info.Instructions.Store(sc.Instructions)
	identity := session.Identity{
		SessionID: "looptest",
		Agent:     info.Name,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := session.New("looptest", identity, info, rec,
		noAgents{}, noScheduler{}, noUsers{}, safety.New(safeProfile), handlers, pool.New(2), log)
	a.LoopFile = watch
	a.LoopSrc = src
	a.SupportSrcs = support
	a.TracePayloads = true
	a.Tracer = tc.record

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.Run(runCtx)

	for i, in := range msgs {
		if runCtx.Err() != nil {
			break
		}
		from := in.From
		if from == "" {
			from = "tester"
		}
		turnID := fmt.Sprintf("scenario-%d", i)
		errBefore := tc.errCount()
		a.Mailbox <- session.Message{
			ID:      turnID,
			Type:    "user",
			From:    "user:" + from,
			Text:    in.Text,
			Channel: "test",
			ReplyTo: "reply",
			Ts:      time.Now().Unix(),
		}
		if !waitForTurnOrError(runCtx, tc, turnID, errBefore, timeout) {
			break
		}
	}
	// Give any final asynchronous send a moment to land.
	if len(sc.Expect) > 0 {
		waitFor(rec, len(sc.Expect), timeout)
	}
	cancel()

	sends := rec.sendsSnapshot()
	gotOps := ops.snapshot()
	events := tc.snapshot()
	if sends == nil {
		sends = []string{}
	}
	if gotOps == nil {
		gotOps = []string{}
	}
	if events == nil {
		events = []session.TraceEvent{}
	}

	assertions, firstErr := evalAssertions(sc, sends, gotOps, events)
	if assertions == nil {
		assertions = []AssertionResult{}
	}
	res := &Result{
		OK:         firstErr == nil,
		Agent:      info.Name,
		Loop:       sc.Loop,
		Model:      info.Model,
		DurationMS: time.Since(started).Milliseconds(),
		Sends:      sends,
		Ops:        gotOps,
		Assertions: assertions,
		Events:     events,
	}
	if firstErr != nil {
		res.Error = firstErr.Error()
	}
	return res, nil
}

func evalAssertions(sc *Scenario, sends, ops []string, events []session.TraceEvent) ([]AssertionResult, error) {
	var out []AssertionResult
	var firstErr error
	recordErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	if len(sc.Expect) > 0 {
		if len(sends) != len(sc.Expect) {
			err := fmt.Errorf("expected %d send(s), got %d: %q", len(sc.Expect), len(sends), sends)
			recordErr(err)
			out = append(out, AssertionResult{
				Kind:     "expect",
				Index:    0,
				Expected: fmt.Sprintf("%d send(s)", len(sc.Expect)),
				Actual:   fmt.Sprintf("%d send(s)", len(sends)),
				OK:       false,
				Err:      err.Error(),
			})
		} else {
			for i, exp := range sc.Expect {
				ok := strings.TrimSpace(sends[i]) == strings.TrimSpace(exp)
				ar := AssertionResult{
					Kind:     "expect",
					Index:    i,
					Expected: exp,
					Actual:   sends[i],
					OK:       ok,
				}
				if !ok {
					err := fmt.Errorf("send %d: expected %q, got %q", i+1, exp, sends[i])
					recordErr(err)
					ar.Err = err.Error()
				}
				out = append(out, ar)
			}
		}
	}

	cursor := 0
	for i, want := range sc.ExpectContains {
		found := false
		for cursor < len(sends) {
			if strings.Contains(sends[cursor], want) {
				found = true
				cursor++
				break
			}
			cursor++
		}
		ar := AssertionResult{
			Kind:     "expect_contains",
			Index:    i,
			Expected: want,
			OK:       found,
		}
		if !found {
			err := fmt.Errorf("no send contains %q (got %q)", want, sends)
			recordErr(err)
			ar.Actual = strings.Join(sends, "\n")
			ar.Err = err.Error()
		}
		out = append(out, ar)
	}

	j := 0
	for i, want := range sc.ExpectOps {
		for j < len(ops) && ops[j] != want {
			j++
		}
		found := j < len(ops)
		if found {
			j++
		}
		ar := AssertionResult{
			Kind:     "expect_ops",
			Index:    i,
			Expected: want,
			OK:       found,
		}
		if !found {
			err := fmt.Errorf("expected op %q; saw %v", want, ops)
			recordErr(err)
			ar.Actual = strings.Join(ops, ", ")
			ar.Err = err.Error()
		}
		out = append(out, ar)
	}

	if len(out) == 0 {
		for _, ev := range events {
			if ev.Kind == "error" && ev.Err != "" {
				err := fmt.Errorf("loop error: %s", ev.Err)
				recordErr(err)
				out = append(out, AssertionResult{
					Kind:     "runtime",
					Index:    0,
					Expected: "no loop errors",
					Actual:   ev.Err,
					OK:       false,
					Err:      err.Error(),
				})
				break
			}
		}
	}

	return out, firstErr
}

type traceCollector struct {
	seq    atomic.Int64
	mu     sync.Mutex
	events []session.TraceEvent
	ended  map[string]bool
	errs   int
}

func (c *traceCollector) record(ev session.TraceEvent) {
	ev.Seq = c.seq.Add(1)
	c.mu.Lock()
	if c.ended == nil {
		c.ended = map[string]bool{}
	}
	c.events = append(c.events, ev)
	if ev.Kind == "turn.end" && ev.Turn != "" {
		c.ended[ev.Turn] = true
	}
	if ev.Kind == "error" {
		c.errs++
	}
	c.mu.Unlock()
}

func (c *traceCollector) turnDone(turn string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ended[turn]
}

func (c *traceCollector) errCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errs
}

func (c *traceCollector) snapshot() []session.TraceEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]session.TraceEvent(nil), c.events...)
}

func waitForTurnOrError(ctx context.Context, tc *traceCollector, turn string, errBefore int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if tc.turnDone(turn) || tc.errCount() > errBefore {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func waitFor(rec *recorder, n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rec.count() >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

type recorder struct {
	mu    sync.Mutex
	sends []string
}

func (r *recorder) Send(channel, replyTo, text string, attachments []media.Part) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, text)
	return nil
}

// SendReply records a structured answer the same way as Send: a scenario's
// expect list is about the text the loop produced.
func (r *recorder) SendReply(channel, replyTo string, rep gateway.Reply) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, rep.Text)
	return nil
}

func (r *recorder) SendDelta(channel, replyTo, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, text)
	return nil
}

func (r *recorder) EndTurn(channel, replyTo string, err error) {}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sends)
}

func (r *recorder) sendsSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sends...)
}

type opLog struct {
	mu   sync.Mutex
	list []string
}

func (o *opLog) add(kind string) {
	o.mu.Lock()
	o.list = append(o.list, kind)
	o.mu.Unlock()
}

func (o *opLog) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.list...)
}

// scenarioHandlers builds the deterministic op surface. A loop that calls an
// op with no canned response gets a clear error rather than a hang or a real
// network request.
func scenarioHandlers(sc *Scenario, rec *recorder) (map[string]session.OpHandler, *opLog) {
	log := &opLog{}
	fail := func(kind string) (string, bool) {
		b, _ := json.Marshal(map[string]any{
			"error": "looptest: op " + kind + " is not stubbed; add it under ops: or use a tool",
		})
		return string(b), false
	}
	ok := func(v any) (string, bool) {
		b, err := json.Marshal(v)
		if err != nil {
			return fail("encode")
		}
		return string(b), true
	}
	handlers := map[string]session.OpHandler{
		"llm.chat": func(ctx context.Context, op session.Op) (string, bool) {
			log.add(op.Type)
			reply := pickReply(sc.LLM, op.Messages)
			payload := map[string]any{"text": reply.Text, "tool_calls": reply.ToolCalls}
			if reply.Usage != nil {
				payload["usage"] = reply.Usage
			}
			return ok(payload)
		},
		"tools.list": func(ctx context.Context, op session.Op) (string, bool) {
			log.add(op.Type)
			names := make([]map[string]any, 0, len(sc.Tools))
			for name := range sc.Tools {
				names = append(names, map[string]any{
					"name":        name,
					"description": "scenario tool",
					"parameters":  map[string]any{"type": "object"},
				})
			}
			return ok(names)
		},
		"tools.run": func(ctx context.Context, op session.Op) (string, bool) {
			log.add(op.Type)
			v, found := sc.Tools[op.Tool]
			if !found {
				return fail("tools.run " + op.Tool)
			}
			return ok(v)
		},
	}
	for kind, response := range sc.Ops {
		kind, response := kind, response
		handlers[kind] = func(ctx context.Context, op session.Op) (string, bool) {
			log.add(op.Type)
			return response, true
		}
	}
	// A few read-only ops are safe to answer generically; anything else is
	// explicit in the scenario.
	for _, kind := range []string{"store.query", "store.list", "store.scopes", "files.list", "files.projects"} {
		if _, exists := handlers[kind]; !exists {
			handlers[kind] = func(ctx context.Context, op session.Op) (string, bool) {
				log.add(op.Type)
				return ok([]any{})
			}
		}
	}
	for _, kind := range []string{"store.put", "store.delete", "files.put", "files.delete", "files.commit", "files.checkout", "scheduler.every", "scheduler.after", "scheduler.cron", "scheduler.cancel"} {
		if _, exists := handlers[kind]; !exists {
			handlers[kind] = func(ctx context.Context, op session.Op) (string, bool) {
				log.add(op.Type)
				return ok(map[string]any{"ok": true, "id": "scenario-timer"})
			}
		}
	}
	_ = rec
	return handlers, log
}

func pickReply(replies []LLMReply, messages []session.ChatMessage) LLMReply {
	if len(replies) == 0 {
		return LLMReply{Text: "scenario reply"}
	}
	for _, r := range replies {
		if r.Match == "" {
			continue
		}
		for _, m := range messages {
			if strings.Contains(m.Content, r.Match) {
				return r
			}
		}
	}
	for _, r := range replies {
		if r.Match == "" {
			return r
		}
	}
	return replies[len(replies)-1]
}

func first(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// noAgents is the empty multi-agent surface: a scenario that delegates should
// stub the operation it actually depends on rather than pretend to have a
// second session.
type noAgents struct{}

func (noAgents) Send(context.Context, session.Identity, string, map[string]any) error {
	return fmt.Errorf("looptest: agent.send is not available in a scenario")
}
func (noAgents) Request(context.Context, session.Identity, string, map[string]any, time.Duration) (session.Message, error) {
	return session.Message{}, fmt.Errorf("looptest: agent.request is not available in a scenario")
}
func (noAgents) Reply(context.Context, session.Identity, string, map[string]any) error {
	return fmt.Errorf("looptest: agent.reply is not available in a scenario")
}
func (noAgents) Spawn(context.Context, session.Identity, string, map[string]any) (session.SpawnResult, error) {
	return session.SpawnResult{}, fmt.Errorf("looptest: agent.spawn is not available in a scenario")
}
func (noAgents) List(session.Identity) []session.AgentSummary { return nil }

type noScheduler struct{}

func (noScheduler) Every(string, float64) (string, error) { return "scenario-timer", nil }
func (noScheduler) After(string, float64) (string, error) { return "scenario-timer", nil }
func (noScheduler) Cron(string, string) (string, error)   { return "scenario-timer", nil }
func (noScheduler) Cancel(string, string) error           { return nil }

type noUsers struct{}

func (noUsers) LookupUser(string) (string, string, bool) { return "", "", false }

// Load parses a scenario from bytes. Relative paths resolve against the
// process CWD; LoadFile resolves them against the scenario's directory.
func Load(b []byte) (*Scenario, error) {
	return LoadFile("", b)
}

// LoadFile parses a scenario file and anchors its relative paths to the file's
// directory.
func LoadFile(path string, b []byte) (*Scenario, error) {
	var sc Scenario
	if err := yaml.Unmarshal(b, &sc); err != nil {
		return nil, err
	}
	if path != "" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		sc.baseDir = filepath.Dir(abs)
	}
	return &sc, nil
}

// resolve joins a relative path to base unless it is absolute or a namespaced
// reference (plugin:, package:).
func resolve(p, base string) string {
	if p == "" || base == "" || filepath.IsAbs(p) ||
		strings.HasPrefix(p, "plugin:") || strings.HasPrefix(p, "package:") {
		return p
	}
	return filepath.Join(base, p)
}
