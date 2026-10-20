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
	"time"

	"gopkg.in/yaml.v3"

	"agentflow/internal/builtins"
	"agentflow/internal/core/media"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/safety"
	"agentflow/internal/core/session"
)

// Scenario is the YAML shape. Only Loop is required.
type Scenario struct {
	Agent        string            `yaml:"agent"`
	Loop         string            `yaml:"loop"`
	Instructions string            `yaml:"instructions"`
	Model        string            `yaml:"model"`
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

type LLMReply struct {
	Match     string           `yaml:"match"`
	Text      string           `yaml:"text"`
	ToolCalls []map[string]any `yaml:"tool_calls"`
}

// Run executes a scenario and returns a readable failure. It never panics on a
// loop error: the actor logs it and the expectation check reports what the
// loop actually produced.
func Run(sc *Scenario, out io.Writer) error {
	if sc.Loop == "" {
		return fmt.Errorf("scenario: loop is required")
	}
	if len(sc.Messages) == 0 {
		sc.Messages = []Input{{From: "tester", Text: "hello"}}
	}
	timeout := 3 * time.Second
	if sc.Timeout != "" {
		d, err := time.ParseDuration(sc.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("scenario: timeout %q is not a positive duration", sc.Timeout)
		}
		timeout = d
	}
	loopRef := resolve(sc.Loop, sc.baseDir)
	if sc.Packages != nil {
		resolved := make(map[string]string, len(sc.Packages))
		for name, dir := range sc.Packages {
			resolved[name] = resolve(dir, sc.baseDir)
		}
		builtins.SetPackages(resolved)
	}
	src, watch, err := builtins.Resolve(loopRef)
	if err != nil {
		return fmt.Errorf("loop: %w", err)
	}
	support, err := builtins.SupportChunks()
	if err != nil {
		return fmt.Errorf("support chunks: %w", err)
	}

	rec := &recorder{}
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
		noAgents{}, noScheduler{}, noUsers{}, safety.New(safety.None), handlers, pool.New(2), log)
	a.LoopFile = watch
	a.LoopSrc = src
	a.SupportSrcs = support

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	for i, in := range sc.Messages {
		from := in.From
		if from == "" {
			from = "tester"
		}
		before := rec.count()
		a.Mailbox <- session.Message{
			ID:      fmt.Sprintf("scenario-%d", i),
			Type:    "user",
			From:    "user:" + from,
			Text:    in.Text,
			Channel: "test",
			ReplyTo: "reply",
			Ts:      time.Now().Unix(),
		}
		// If this turn is expected to produce output, wait for it before
		// sending the next input; otherwise give the loop a short turn.
		if len(sc.Expect) > before {
			if !waitFor(rec, before+1, timeout) {
				break
			}
		} else {
			time.Sleep(30 * time.Millisecond)
		}
	}
	// Give a final asynchronous send a moment to land.
	waitFor(rec, len(sc.Expect), timeout)
	cancel()

	sends := rec.sendsSnapshot()
	gotOps := ops.snapshot()
	if err := checkSends(sc, sends); err != nil {
		return err
	}
	if err := checkOps(sc, gotOps); err != nil {
		return err
	}
	if out != nil {
		fmt.Fprintf(out, "ok: %s (%d sends, %d ops)\n", sc.Loop, len(sends), len(gotOps))
		for _, s := range sends {
			fmt.Fprintf(out, "  -> %s\n", s)
		}
	}
	return nil
}

func checkSends(sc *Scenario, sends []string) error {
	if len(sc.Expect) > 0 {
		if len(sends) != len(sc.Expect) {
			return fmt.Errorf("expected %d send(s), got %d: %q", len(sc.Expect), len(sends), sends)
		}
		for i := range sc.Expect {
			if strings.TrimSpace(sends[i]) != strings.TrimSpace(sc.Expect[i]) {
				return fmt.Errorf("send %d: expected %q, got %q", i+1, sc.Expect[i], sends[i])
			}
		}
	}
	cursor := 0
	for _, want := range sc.ExpectContains {
		found := false
		for cursor < len(sends) {
			if strings.Contains(sends[cursor], want) {
				found = true
				cursor++
				break
			}
			cursor++
		}
		if !found {
			return fmt.Errorf("no send contains %q (got %q)", want, sends)
		}
	}
	return nil
}

func checkOps(sc *Scenario, ops []string) error {
	if len(sc.ExpectOps) == 0 {
		return nil
	}
	j := 0
	for _, want := range sc.ExpectOps {
		for j < len(ops) && ops[j] != want {
			j++
		}
		if j == len(ops) {
			return fmt.Errorf("expected op %q; saw %v", want, ops)
		}
		j++
	}
	return nil
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
			return ok(map[string]any{"text": reply.Text, "tool_calls": reply.ToolCalls})
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
