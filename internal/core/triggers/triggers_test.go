package triggers

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/session"
)

// syncBuf is a goroutine-safe log sink (job goroutines log while the test
// reads).
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// record is one delivered trigger message.
type record struct {
	agent, key string
	msg        session.Message
}

// collector stands in for supervisor.Deliver.
type collector struct {
	mu  sync.Mutex
	got []record
}

func (c *collector) deliver(agent, key string, msg session.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, record{agent: agent, key: key, msg: msg})
	return nil
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

func (c *collector) records() []record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]record(nil), c.got...)
}

// waitFor polls until cond is true or the deadline passes.
func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

func newHarness(t *testing.T, known map[string]string, logs *syncBuf) (*Service, *collector) {
	t.Helper()
	if logs == nil {
		logs = &syncBuf{}
	}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := &collector{}
	svc := New(func(profile string) (string, bool) {
		agent, ok := known[profile]
		return agent, ok
	}, c.deliver, 0, log)
	svc.poll = 20 * time.Millisecond
	return svc, c
}

func trigger(name, every, cron string, boot bool, target string, payload map[string]any) config.Trigger {
	return config.Trigger{
		Name: name, Every: every, Cron: cron, RunOnBoot: boot,
		Target: config.TriggerTarget{Profile: target}, Payload: payload,
	}
}

// TestEveryFiresOnItsInterval: an every: trigger delivers Message{type:"cron"}
// with the trigger's payload to the resolved target, on its own session key,
// and the first fire comes one interval in — not at boot.
func TestEveryFiresOnItsInterval(t *testing.T) {
	svc, c := newHarness(t, map[string]string{"digest": "digest"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	svc.Reload([]config.Trigger{trigger("morning", "100ms", "", false, "digest", map[string]any{"topic": "news"})})
	defer svc.Stop()

	if svc.Pending() != 1 {
		t.Fatalf("pending = %d; want 1", svc.Pending())
	}
	// Anchoring: nothing before the interval elapses.
	time.Sleep(40 * time.Millisecond)
	if n := c.count(); n != 0 {
		t.Fatalf("fired %d times before the interval elapsed", n)
	}

	waitFor(t, "two fires", 3*time.Second, func() bool { return c.count() >= 2 })

	r := c.records()[0]
	if r.agent != "digest" || r.key != "morning" {
		t.Fatalf("delivered to %q/%q; want digest/morning", r.agent, r.key)
	}
	if r.msg.Type != "cron" {
		t.Fatalf("type = %q; want cron", r.msg.Type)
	}
	if r.msg.Payload["topic"] != "news" {
		t.Fatalf("payload = %v; want the trigger payload", r.msg.Payload)
	}
	if !strings.HasPrefix(r.msg.ID, "cron:morning:") {
		t.Fatalf("id = %q; want a cron:<name>:<ts> id", r.msg.ID)
	}
	if r.msg.Provenance == nil || r.msg.Provenance.Kind != "scheduler" || r.msg.Provenance.Principal != "trigger:morning" {
		t.Fatalf("provenance = %+v", r.msg.Provenance)
	}
	if r.msg.Ts == 0 {
		t.Fatal("ts must be stamped")
	}
}

// TestRunOnBootFiresImmediately: run_on_boot fires the trigger at startup
// without waiting for the interval — for every: and cron: alike.
func TestRunOnBootFiresImmediately(t *testing.T) {
	svc, c := newHarness(t, map[string]string{"digest": "digest"}, nil)
	svc.Start(context.Background())
	defer svc.Stop()
	svc.Reload([]config.Trigger{
		trigger("hourly", "6h", "", true, "digest", map[string]any{"kind": "every"}),
		trigger("daily", "", "0 9 * * *", true, "digest", map[string]any{"kind": "cron"}),
	})

	waitFor(t, "both boot fires", 2*time.Second, func() bool { return c.count() >= 2 })
	names := map[string]bool{}
	for _, r := range c.records() {
		names[r.msg.Payload["kind"].(string)] = true
	}
	if !names["every"] || !names["cron"] {
		t.Fatalf("boot fires = %v; want both kinds", names)
	}
	// No further fire from either: the intervals are hours away.
	time.Sleep(50 * time.Millisecond)
	if n := c.count(); n != 2 {
		t.Fatalf("fires = %d; want exactly the two boot fires", n)
	}
}

// TestUnschedulableTriggersAreSkipped: a malformed or unresolvable declaration
// is logged and skipped — never a boot failure, and never a silent skip.
func TestUnschedulableTriggersAreSkipped(t *testing.T) {
	cases := []struct {
		name    string
		trigger config.Trigger
		wantLog string
	}{
		{"bad every", trigger("a", "5 minutes", "", false, "digest", nil), "bad every interval"},
		{"too fast", trigger("b", "10ms", "", false, "digest", nil), "below the minimum"},
		{"bad cron", trigger("c", "", "0 9 * * ", false, "digest", nil), "bad cron expression"},
		{"both schedules", trigger("d", "5m", "0 9 * * *", false, "digest", nil), "mutually exclusive"},
		{"unknown target", trigger("e", "5m", "", false, "ghost", nil), "neither a configured agent nor a spawn profile"},
		{"no target", trigger("f", "5m", "", false, "", nil), "no target profile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &syncBuf{}
			svc, c := newHarness(t, map[string]string{"digest": "digest"}, logs)
			svc.Start(context.Background())
			defer svc.Stop()
			svc.Reload([]config.Trigger{tc.trigger})
			if svc.Pending() != 0 {
				t.Fatalf("pending = %d; want 0", svc.Pending())
			}
			time.Sleep(30 * time.Millisecond)
			if c.count() != 0 {
				t.Fatal("a skipped trigger must not fire")
			}
			if got := logs.String(); !strings.Contains(got, tc.wantLog) {
				t.Fatalf("log missing %q:\n%s", tc.wantLog, got)
			}
		})
	}
}

// TestEventTriggersAreNotScheduled: event: triggers belong to the router and
// loops — the scheduler must leave them alone and say nothing.
func TestEventTriggersAreNotScheduled(t *testing.T) {
	logs := &syncBuf{}
	svc, c := newHarness(t, map[string]string{"digest": "digest"}, logs)
	svc.Start(context.Background())
	defer svc.Stop()
	svc.Reload([]config.Trigger{{
		Name:   "ticket",
		Event:  &config.TriggerEvent{Channel: "webhook", Match: "urgent"},
		Target: config.TriggerTarget{Profile: "digest"},
	}})
	if svc.Pending() != 0 {
		t.Fatalf("pending = %d; want 0", svc.Pending())
	}
	time.Sleep(30 * time.Millisecond)
	if c.count() != 0 {
		t.Fatal("an event trigger must not be scheduled")
	}
	if got := logs.String(); strings.Contains(got, "level=WARN") {
		t.Fatalf("an event trigger must not warn:\n%s", got)
	}
}

// TestReloadDiffsTheLiveSet: an unchanged declaration keeps its timer (its
// anchor is not reset), a changed one is rescheduled, and a removed one stops.
func TestReloadDiffsTheLiveSet(t *testing.T) {
	logs := &syncBuf{}
	svc, c := newHarness(t, map[string]string{"digest": "digest"}, logs)
	svc.Start(context.Background())
	defer svc.Stop()

	list := []config.Trigger{trigger("beat", "6h", "", false, "digest", map[string]any{"v": "1"})}
	svc.Reload(list)
	// Identical reload: the live timer is kept (no reschedule line).
	svc.Reload([]config.Trigger{trigger("beat", "6h", "", false, "digest", map[string]any{"v": "1"})})
	if got := logs.String(); strings.Contains(got, "trigger rescheduled") {
		t.Fatalf("an unchanged trigger must not be rescheduled:\n%s", got)
	}
	if svc.Pending() != 1 {
		t.Fatalf("pending = %d; want 1", svc.Pending())
	}

	// A changed interval reschedules.
	svc.Reload([]config.Trigger{trigger("beat", "1h", "", false, "digest", map[string]any{"v": "2"})})
	if got := logs.String(); !strings.Contains(got, "trigger rescheduled") {
		t.Fatalf("a changed trigger must be rescheduled:\n%s", got)
	}

	// A changed payload reschedules too (the declaration is the unit).
	svc.Reload([]config.Trigger{trigger("beat", "1h", "", false, "digest", map[string]any{"v": "3"})})
	if svc.Pending() != 1 {
		t.Fatalf("pending = %d; want 1", svc.Pending())
	}

	// Removal stops it: no fire can arrive afterwards.
	svc.Reload(nil)
	if svc.Pending() != 0 {
		t.Fatalf("pending = %d; want 0", svc.Pending())
	}
	before := c.count()
	time.Sleep(50 * time.Millisecond)
	if c.count() != before {
		t.Fatal("a removed trigger must not fire")
	}
}

// TestReloadBeforeStartIsLoud: Reload without Start schedules nothing and says
// so, instead of silently dropping the trigger set.
func TestReloadBeforeStartIsLoud(t *testing.T) {
	logs := &syncBuf{}
	svc, _ := newHarness(t, map[string]string{"digest": "digest"}, logs)
	svc.Reload([]config.Trigger{trigger("a", "5m", "", false, "digest", nil)})
	if svc.Pending() != 0 {
		t.Fatal("nothing may be scheduled before Start")
	}
	if !strings.Contains(logs.String(), "before Start") {
		t.Fatalf("missing the loud error:\n%s", logs.String())
	}
}

// TestWatchConfigDirPicksUpEdits: the watcher re-reads triggers/*.yaml and
// applies the new set without a restart.
func TestWatchConfigDirPicksUpEdits(t *testing.T) {
	dir := t.TempDir()
	write := func(triggers string) {
		if err := os.MkdirAll(filepath.Join(dir, "triggers"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "triggers", "a.yaml"), []byte(triggers), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	system := "version: \"1\"\ngateway:\n  listen: \":0\"\n"
	if err := os.WriteFile(filepath.Join(dir, "system.yaml"), []byte(system), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profiles", "bot.yaml"), []byte("name: bot\nloop: builtin:per_chat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write(`
triggers:
  - { name: alpha, every: 6h, run_on_boot: true, target: { profile: bot }, payload: { tag: alpha } }
`)

	cfg, err := config.LoadDir(dir, slog.New(slog.NewTextHandler(&syncBuf{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Triggers) != 1 || cfg.Triggers[0].Name != "alpha" {
		t.Fatalf("configdir triggers = %+v", cfg.Triggers)
	}

	logs := &syncBuf{}
	svc, c := newHarness(t, map[string]string{"bot": "bot"}, logs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	svc.Reload(cfg.Triggers)
	defer svc.Stop()
	waitFor(t, "the alpha boot fire", 2*time.Second, func() bool { return c.count() >= 1 })

	// A new trigger file, one poll later.
	write(`
triggers:
  - { name: beta, every: 6h, run_on_boot: true, target: { profile: bot }, payload: { tag: beta } }
`)
	go svc.WatchConfigDir(ctx, dir)
	waitFor(t, "the beta fire after reload", 3*time.Second, func() bool {
		for _, r := range c.records() {
			if r.msg.Payload["tag"] == "beta" {
				return true
			}
		}
		return false
	})
	if !strings.Contains(logs.String(), "triggers reloaded") {
		t.Fatalf("reload not logged:\n%s", logs.String())
	}

	// A malformed fragment keeps the running set rather than emptying it (the
	// strict decoder rejects the unknown field).
	write("triggers:\n  - { name: gamma, every: 6h, bogus_field: 1 }\n")
	waitFor(t, "the reload failure warning", 3*time.Second, func() bool {
		return strings.Contains(logs.String(), "trigger reload failed")
	})
	if svc.Pending() != 1 {
		t.Fatalf("pending = %d; want the surviving beta trigger", svc.Pending())
	}
}

// TestStopCancelsEverything: Stop leaves no live trigger behind.
func TestStopCancelsEverything(t *testing.T) {
	svc, c := newHarness(t, map[string]string{"digest": "digest"}, nil)
	svc.Start(context.Background())
	svc.Reload([]config.Trigger{trigger("tick", "100ms", "", false, "digest", nil)})
	waitFor(t, "a fire", 2*time.Second, func() bool { return c.count() >= 1 })
	svc.Stop()
	time.Sleep(20 * time.Millisecond)
	before := c.count()
	time.Sleep(200 * time.Millisecond)
	if c.count() != before {
		t.Fatal("Stop must cancel every trigger")
	}
}
