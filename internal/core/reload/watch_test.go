package reload

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/builtins"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/vm"
)

// directive is the prelude version line every Lua fixture has to carry: the
// agent's loop is refused at boot and at reload without it, and the fixtures
// here are reloaded rather than only read. Spelling it from vm.PreludeVersion
// keeps a version bump from turning every fixture in this file red for a reason
// that has nothing to do with what it tests — the chunks this repo ships spell
// the number literally, and the conformance suite is what pins those.
func directive() string { return fmt.Sprintf("-- af-prelude-version: %d\n", vm.PreludeVersion) }

// syncBuf is a goroutine-safe log sink (the watcher polls on its own goroutine
// while the test reads).
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

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

// agentsNamed returns the agents whose log line contains msg, read from the
// named attribute ("agent=" / "session=").
func agentsNamed(logs, msg string, attr string, agents ...string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, msg) {
			continue
		}
		for _, a := range agents {
			if strings.Contains(line, attr+a) {
				out[a] = true
			}
		}
	}
	return out
}

// dirLoop is a shared-loop-directory fixture: one loop directory, one agent per
// name bound to it, each with a live session parked on its mailbox (where a
// reload lands).
type dirLoop struct {
	dir  string
	sup  *supervisor.Supervisor
	logs *syncBuf
	log  *slog.Logger
}

func newDirLoop(t *testing.T, names ...string) *dirLoop {
	t.Helper()
	dir := t.TempDir()
	writeMember(t, dir, "10-main.lua", directive()+"function loop() while true do session.inbox() end end\n")
	writeMember(t, dir, "20-tools.lua", directive()+"-- tools member\n")

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	loopSrc := "function loop() while true do session.inbox() end end"

	defs := map[string]*supervisor.AgentDef{}
	for _, name := range names {
		defs[name] = &supervisor.AgentDef{
			Info:         &session.Info{Name: name, HistoryBudget: 100},
			Capabilities: map[string]bool{},
			Handlers:     map[string]session.OpHandler{},
			LoopFile:     dir,
			LoopSrc:      loopSrc,
		}
	}

	sup := supervisor.New(defs, gateway.NewRegistry(log), pool.New(2), nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sup.Start(ctx)
	for _, name := range names {
		if err := sup.Deliver(name, "k", session.Message{ID: "m1", Type: "user", From: "u", Text: "hi"}); err != nil {
			t.Fatalf("deliver to %s: %v", name, err)
		}
	}
	// Wait for each session's first loop load to finish before handing the
	// fixture over: every test here edits these files immediately, and on
	// Windows a file the actor is still reading cannot be removed.
	for _, name := range names {
		skey := "session=" + name + "|k"
		waitFor(t, "session "+name+" to load its loop", 10*time.Second, func() bool {
			return loggedLine(logs.String(), "loop started", skey)
		})
	}
	return &dirLoop{dir: dir, sup: sup, logs: logs, log: log}
}

// loggedLine reports whether some single log line contains every fragment.
func loggedLine(logs string, fragments ...string) bool {
	for _, line := range strings.Split(logs, "\n") {
		all := true
		for _, f := range fragments {
			if !strings.Contains(line, f) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func writeMember(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// touch advances a file's mtime explicitly: a same-second write is ambiguous on
// coarse-mtime filesystems.
func touch(t *testing.T, path string) {
	t.Helper()
	newer := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, newer, newer); err != nil {
		t.Fatal(err)
	}
}

// promptFixture is a prompt-registry fixture: one registry instance shared by
// every agent (exactly what main.go wires), one file-backed key, one inline
// key, and a live session per agent parked on its mailbox.
type promptFixture struct {
	dir   string
	file  string
	sup   *supervisor.Supervisor
	reg   *session.PromptRegistry
	files map[string]string
	defs  map[string]*supervisor.AgentDef
	logs  *syncBuf
	log   *slog.Logger
}

const (
	promptFirst  = "first version\n"
	promptSecond = "second version\n"
	promptInline = "[Knowledge base]"
)

func newPromptFixture(t *testing.T, names ...string) *promptFixture {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "assistant.md")
	if err := os.WriteFile(file, []byte(promptFirst), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	reg := session.NewPromptRegistry(map[string]string{
		"assistant_system": promptFirst,
		"kb_header":        promptInline,
	})
	files := map[string]string{"assistant_system": file}

	defs := map[string]*supervisor.AgentDef{}
	for _, name := range names {
		box := &session.StringBox{}
		box.Store(promptFirst)
		defs[name] = &supervisor.AgentDef{
			Info: &session.Info{
				Name:               name,
				HistoryBudget:      100,
				Prompts:            reg,
				InstructionsPrompt: "assistant_system",
				Instructions:       box,
			},
			Capabilities: map[string]bool{},
			Handlers:     map[string]session.OpHandler{},
			LoopSrc:      "function loop() while true do session.inbox() end end",
		}
	}

	sup := supervisor.New(defs, gateway.NewRegistry(log), pool.New(2), nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sup.Start(ctx)
	for _, name := range names {
		if err := sup.Deliver(name, "k", session.Message{ID: "m1", Type: "user", From: "u", Text: "hi"}); err != nil {
			t.Fatalf("deliver to %s: %v", name, err)
		}
	}
	for _, name := range names {
		skey := "session=" + name + "|k"
		waitFor(t, "session "+name+" to load its loop", 10*time.Second, func() bool {
			return loggedLine(logs.String(), "loop started", skey)
		})
	}
	return &promptFixture{dir: dir, file: file, sup: sup, reg: reg, files: files, defs: defs, logs: logs, log: log}
}

// startWatcher runs the watcher against the fixture's prompt source.
func (fx *promptFixture) startWatcher(t *testing.T) *Watcher {
	t.Helper()
	w := New(fx.sup, &PromptSource{Registry: fx.reg, Files: fx.files}, fx.log)
	w.Start()
	t.Cleanup(w.Stop)
	return w
}

// editPrompt rewrites the backing file and forces a distinct mtime.
func (fx *promptFixture) editPrompt(t *testing.T, text string) {
	t.Helper()
	if err := os.WriteFile(fx.file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, fx.file)
}

// countLogged counts log lines containing every fragment.
func countLogged(logs string, fragments ...string) int {
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		all := true
		for _, f := range fragments {
			if !strings.Contains(line, f) {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}

// TestPromptFileReloadsEveryAgent: editing a file-backed prompt republishes it
// to the deployment-wide registry, so every agent's agent.config() text for
// that key reflects the new content. The registry is one shared instance, and
// the poll bookkeeping is keyed by prompt name rather than by agent — keyed
// per agent (as loop paths are), whichever agent poll() reached first would
// consume the change and the rest would keep the stale text.
func TestPromptFileReloadsEveryAgent(t *testing.T) {
	fx := newPromptFixture(t, "alpha", "beta")
	fx.startWatcher(t)

	fx.editPrompt(t, promptSecond)

	waitFor(t, "the shared registry to pick up the new text", 10*time.Second, func() bool {
		s, _ := fx.reg.Get("assistant_system")
		return s == promptSecond
	})
	// Every agent reads the same registry instance.
	for name, def := range fx.defs {
		if got, _ := def.Info.Prompts.Get("assistant_system"); got != promptSecond {
			t.Fatalf("agent %s still sees %q", name, got)
		}
	}
	if _, ok := fx.reg.Get("kb_header"); !ok {
		t.Fatal("an unrelated prompt key disappeared")
	}
	if !loggedLine(fx.logs.String(), "reload: prompt updated", "prompt=assistant_system") {
		t.Fatalf("prompt reload not logged with its key and file:\n%s", fx.logs.String())
	}
}

// TestPromptFileReloadsInstructionsInPlace: an agent whose system prompt came
// from a prompt key gets the new text in place — no session restart, so the
// running loop keeps its state and picks the text up on its next turn.
func TestPromptFileReloadsInstructionsInPlace(t *testing.T) {
	fx := newPromptFixture(t, "alpha")
	fx.startWatcher(t)

	fx.editPrompt(t, promptSecond)

	waitFor(t, "the agent's system prompt to update in place", 10*time.Second, func() bool {
		s, _ := fx.reg.Get("assistant_system")
		return s == promptSecond && fx.defs["alpha"].Info.Instructions.Load() == promptSecond
	})
	if !loggedLine(fx.logs.String(), "reload: instructions updated", "agent=alpha") {
		t.Fatalf("instructions update not logged:\n%s", fx.logs.String())
	}
	if strings.Contains(fx.logs.String(), "hot reload: restarting loop") {
		t.Fatalf("a prompt change must not restart the session:\n%s", fx.logs.String())
	}
}

// TestPromptInlineEntryIsNotWatched: an inline:/text: entry has no backing
// file, so it is polled by nothing and produces no reload — and in particular
// is never blanked by a read that has no file to make.
func TestPromptInlineEntryIsNotWatched(t *testing.T) {
	fx := newPromptFixture(t, "alpha")
	fx.startWatcher(t)

	// Several poll ticks (the watcher ticks at 500ms).
	time.Sleep(1600 * time.Millisecond)

	if strings.Contains(fx.logs.String(), "reload: prompt updated") {
		t.Fatalf("an inline prompt must not be watched:\n%s", fx.logs.String())
	}
	if got, _ := fx.reg.Get("kb_header"); got != promptInline {
		t.Fatalf("inline prompt text changed: %q", got)
	}
}

// TestPromptUnreadableKeepsPreviousText: a prompt file that cannot be re-read
// warns and keeps the text already published — never an empty prompt. Both
// ways it can fail are covered: the path is gone (stat fails) and the path is
// no longer a readable file (stat succeeds, read fails).
func TestPromptUnreadableKeepsPreviousText(t *testing.T) {
	cases := map[string]func(t *testing.T, fx *promptFixture){
		"removed": func(t *testing.T, fx *promptFixture) {
			if err := os.Remove(fx.file); err != nil {
				t.Fatal(err)
			}
		},
		"no longer a file": func(t *testing.T, fx *promptFixture) {
			if err := os.Remove(fx.file); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(fx.file, 0o750); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newPromptFixture(t, "alpha")
			fx.startWatcher(t)

			break_(t, fx)

			waitFor(t, "the failure to be reported", 10*time.Second, func() bool {
				return loggedLine(fx.logs.String(), "reload: cannot read prompt", "prompt=assistant_system")
			})
			// Long enough for several more polls: the previous text must hold,
			// and the warning must not repeat on every tick.
			time.Sleep(1600 * time.Millisecond)

			if got, _ := fx.reg.Get("assistant_system"); got != promptFirst {
				t.Fatalf("previous prompt text not retained: %q", got)
			}
			if fx.defs["alpha"].Info.Instructions.Load() != promptFirst {
				t.Fatal("previous instructions not retained")
			}
			if n := countLogged(fx.logs.String(), "reload: cannot read prompt"); n != 1 {
				t.Fatalf("unreadable prompt warned %d times; want exactly 1 (no warn storm)", n)
			}
		})
	}
}

// TestUnchangedPromptDoesNotReload: an untouched prompt file stays quiet
// across polls — the mtime is seeded at Start, so only a real edit fires.
func TestUnchangedPromptDoesNotReload(t *testing.T) {
	fx := newPromptFixture(t, "alpha")
	fx.startWatcher(t)

	time.Sleep(1600 * time.Millisecond)

	if strings.Contains(fx.logs.String(), "reload: prompt updated") {
		t.Fatalf("an untouched prompt must not reload:\n%s", fx.logs.String())
	}
}

// TestSharedPromptKeyUpdatesBothAgents: two agents sourced from one prompt key
// both observe a single edit — one file edit, one republish, both system
// prompts updated. The change must not be consumed by only the first agent
// polled.
func TestSharedPromptKeyUpdatesBothAgents(t *testing.T) {
	fx := newPromptFixture(t, "alpha", "beta")
	fx.startWatcher(t)

	fx.editPrompt(t, promptSecond)

	waitFor(t, "both agents to pick up the new system prompt", 10*time.Second, func() bool {
		return fx.defs["alpha"].Info.Instructions.Load() == promptSecond &&
			fx.defs["beta"].Info.Instructions.Load() == promptSecond
	})
	if n := countLogged(fx.logs.String(), "reload: prompt updated"); n != 1 {
		t.Fatalf("one edit produced %d reloads; want exactly 1", n)
	}
}

// TestSharedLoopPathReloadsEveryAgent: two agents bound to one loop directory
// must both reload when a member changes. The watcher's mtime bookkeeping is
// per (agent, path) — keyed by path alone, whichever agent poll() visited first
// would consume the change and the other would stay on the old loop, so the
// edit applied to only half the deployment, depending on map iteration order.
func TestSharedLoopPathReloadsEveryAgent(t *testing.T) {
	fx := newDirLoop(t, "alpha", "beta")

	w := New(fx.sup, nil, fx.log)
	w.Start()
	defer w.Stop()

	touch(t, filepath.Join(fx.dir, "20-tools.lua"))

	waitFor(t, "both agents to accept the new loop", 10*time.Second, func() bool {
		return len(agentsNamed(fx.logs.String(), "reload: new version accepted", "agent=", "alpha", "beta")) == 2
	})
	waitFor(t, "both sessions to restart", 10*time.Second, func() bool {
		return len(agentsNamed(fx.logs.String(), "hot reload: restarting loop", "session=", "alpha", "beta")) == 2
	})
}

// TestDeletedLoopMemberReloadsEveryAgent: a member that is removed must be a
// change like any other. Its mtime cannot report the deletion — the file is
// simply absent from the next readdir — so the watcher tracks the member set
// per (agent, dir) and treats a vanished member as a change. Without that, a
// directory loop kept running the old concatenated chunk after a file was
// deleted (adding one worked; deleting one did not).
func TestDeletedLoopMemberReloadsEveryAgent(t *testing.T) {
	fx := newDirLoop(t, "alpha", "beta")

	w := New(fx.sup, nil, fx.log)
	w.Start()
	defer w.Stop()

	if err := os.Remove(filepath.Join(fx.dir, "20-tools.lua")); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "both agents to accept the shrunken loop", 10*time.Second, func() bool {
		return len(agentsNamed(fx.logs.String(), "reload: new version accepted", "agent=", "alpha", "beta")) == 2
	})
	waitFor(t, "both sessions to restart", 10*time.Second, func() bool {
		return len(agentsNamed(fx.logs.String(), "hot reload: restarting loop", "session=", "alpha", "beta")) == 2
	})
}

// TestUnchangedDirectoryMembersDoNotReload: the member set is recorded, not
// merely compared — an untouched directory loop must stay quiet across polls,
// or every agent would restart the loop on every tick (a reload storm).
func TestUnchangedDirectoryMembersDoNotReload(t *testing.T) {
	fx := newDirLoop(t, "alpha", "beta")

	w := New(fx.sup, nil, fx.log)
	w.Start()
	defer w.Stop()

	// Several poll ticks (the watcher ticks at 500ms) with nothing touched.
	time.Sleep(1600 * time.Millisecond)
	if got := fx.logs.String(); strings.Contains(got, "reload:") {
		t.Fatalf("an untouched loop directory must not reload:\n%s", got)
	}
}

// TestDeletedMemberEntryIsPruned: the bookkeeping for a removed member is
// dropped in the same pass that reports it, so the maps do not accumulate
// entries for files that no longer exist. This drives changed() directly instead
// of starting the watcher — no poll goroutine, so the state machine is read and
// written from one goroutine and the assertions are deterministic.
func TestDeletedMemberEntryIsPruned(t *testing.T) {
	fx := newDirLoop(t, "alpha")
	gone := filepath.Join(fx.dir, "20-tools.lua")

	w := New(fx.sup, nil, fx.log)
	w.seedDir("alpha", fx.dir) // what Start() does for a directory loop

	if w.changed("alpha", fx.dir) {
		t.Fatal("a freshly seeded directory must not report a change")
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if !w.changed("alpha", fx.dir) {
		t.Fatal("a deleted member must report a change")
	}
	if _, stale := w.mtimes[watchKey{"alpha", gone}]; stale {
		t.Fatalf("mtime for the removed member %s was not pruned", gone)
	}
	if w.members[watchKey{"alpha", fx.dir}][gone] {
		t.Fatalf("removed member %s is still in the member set", gone)
	}
	if w.changed("alpha", fx.dir) {
		t.Fatal("a deletion must be reported once, not on every poll")
	}
}

// TestEmptyDirectoryLoopIsRefused: deleting the last member of a directory loop
// must not install an empty loop. A directory with no members is a condition
// builtins.Resolve rejects — and the actor re-resolves the directory on every
// restart — so "accepting" it here does not yield an idle agent: the restarted
// session cannot load its loop and crash-restarts once a second. The watcher
// must refuse the reload and keep the old version running.
func TestEmptyDirectoryLoopIsRefused(t *testing.T) {
	fx := newDirLoop(t, "alpha")
	// Start from a one-member directory: drop the second member before the
	// watcher seeds, reproducing a loop directory that holds exactly one .lua.
	last := filepath.Join(fx.dir, "10-main.lua")
	if err := os.Remove(filepath.Join(fx.dir, "20-tools.lua")); err != nil {
		t.Fatal(err)
	}

	w := New(fx.sup, nil, fx.log)
	w.Start()
	defer w.Stop()

	if err := os.Remove(last); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the watcher to refuse the empty loop", 10*time.Second, func() bool {
		return strings.Contains(fx.logs.String(), "reload: cannot read loop")
	})
	// Long enough for several polls and for a session that had been restarted
	// onto the empty loop to crash-restart more than once (the actor retries
	// after a second).
	time.Sleep(2500 * time.Millisecond)

	got := fx.logs.String()
	if strings.Contains(got, "reload: new version accepted") {
		t.Fatalf("an empty directory loop must not be accepted:\n%s", got)
	}
	if strings.Contains(got, "cannot read loop plugin") {
		t.Fatalf("the agent entered the crash-restart path:\n%s", got)
	}
	if !strings.Contains(got, "reload: cannot read loop") {
		t.Fatalf("the refusal is not explained:\n%s", got)
	}
}

// TestSharedInstructionsPathUpdatesEveryAgent: the same per-(agent, path)
// keying covers an instructions file shared by several agents — each one's loop
// must see the new content, not just whichever agent poll() visited first.
func TestSharedInstructionsPathUpdatesEveryAgent(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared.md")
	if err := os.WriteFile(shared, []byte("first version\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	defs := map[string]*supervisor.AgentDef{}
	for _, name := range []string{"alpha", "beta"} {
		defs[name] = &supervisor.AgentDef{
			Info:             &session.Info{Name: name, HistoryBudget: 100, Instructions: &session.StringBox{}},
			Capabilities:     map[string]bool{},
			Handlers:         map[string]session.OpHandler{},
			InstructionsPath: shared,
		}
	}

	sup := supervisor.New(defs, gateway.NewRegistry(log), pool.New(1), nil, log)
	sup.Start(context.Background())

	w := New(sup, nil, log)
	w.Start()
	defer w.Stop()

	if err := os.WriteFile(shared, []byte("second version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, shared)

	waitFor(t, "both agents to pick up the instructions", 10*time.Second, func() bool {
		return len(agentsNamed(logs.String(), "reload: instructions updated", "agent=", "alpha", "beta")) == 2
	})
	for name, def := range defs {
		if got := def.Info.Instructions.Load(); got != "second version\n" {
			t.Fatalf("agent %s instructions = %q", name, got)
		}
	}
}

// TestUnchangedPathDoesNotReload: the same guard for a single-file loop.
func TestUnchangedPathDoesNotReload(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "loop.lua")
	if err := os.WriteFile(shared, []byte(directive()+"function loop() end\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	defs := map[string]*supervisor.AgentDef{
		"alpha": {
			Info: &session.Info{Name: "alpha", HistoryBudget: 100}, Capabilities: map[string]bool{},
			Handlers: map[string]session.OpHandler{}, LoopFile: shared,
		},
	}
	sup := supervisor.New(defs, gateway.NewRegistry(log), pool.New(1), nil, log)
	sup.Start(context.Background())

	w := New(sup, nil, log)
	w.Start()
	defer w.Stop()

	time.Sleep(1200 * time.Millisecond)
	if got := logs.String(); strings.Contains(got, "reload:") {
		t.Fatalf("an untouched loop must not reload:\n%s", got)
	}
}

// TestVersionMismatchKeepsRunningLoop: a reload whose chunk targets another
// prelude version is refused, loudly, and the version already running keeps
// serving. Gating the reload path is what makes the gate mean anything — this
// is the path that swaps Lua under a live deployment, and a gate that ran only
// at boot would let hot reload install exactly the chunk it exists to stop.
//
// The stale member is the second one on purpose. A directory loop concatenates
// its members in sorted order, so only the first member's directive leads the
// joined source; a check on the concatenation would read that one and wave the
// whole loop through.
func TestVersionMismatchKeepsRunningLoop(t *testing.T) {
	fx := newDirLoop(t, "alpha")
	stale := fmt.Sprintf("-- af-prelude-version: %d\n-- tools member\n", vm.PreludeVersion+1)

	w := New(fx.sup, nil, fx.log)
	w.Start()
	defer w.Stop()

	writeMember(t, fx.dir, "20-tools.lua", stale)
	touch(t, filepath.Join(fx.dir, "20-tools.lua"))

	waitFor(t, "the watcher to refuse the mismatched loop", 10*time.Second, func() bool {
		// Error level, not Warn: a chunk written against another API is a
		// contract violation, and the operator has a deploy to fix.
		return loggedLine(fx.logs.String(), "level=ERROR", "reload: prelude version mismatch")
	})

	// The member is correct again. The refusal must not be sticky — the next
	// edit reloads as any other edit would.
	writeMember(t, fx.dir, "20-tools.lua", directive()+"-- tools member\n")
	touch(t, filepath.Join(fx.dir, "20-tools.lua"))

	waitFor(t, "the fixed loop to be accepted", 10*time.Second, func() bool {
		return strings.Contains(fx.logs.String(), "reload: new version accepted")
	})
	// One acceptance, and it is the one after the fix: had the mismatched
	// edit been swapped in, the loop would have restarted onto a chunk this
	// core cannot serve and there would be two.
	if n := countLogged(fx.logs.String(), "reload: new version accepted"); n != 1 {
		t.Fatalf("%d loops accepted; want exactly the one written after the fix", n)
	}
}

// routeFixture drives the route half of the watcher the way main wires it: the
// default route ref (plugin:per_chat) shadowed by plugins.dir, the watch path
// Resolve hands back for that shadow, and the sources the watcher was asked to
// install. No supervisor is needed — the route runs in the router's service
// state, not in a session — so this is the poll-and-apply path on its own.
type routeFixture struct {
	file    string
	sup     *supervisor.Supervisor
	logs    *syncBuf
	log     *slog.Logger
	w       *Watcher
	mu      sync.Mutex
	applied []string
}

var (
	routeFirst  = directive() + "function loop() while true do session.inbox() end end -- v1\n"
	routeSecond = directive() + "function loop() while true do session.inbox() end end -- v2\n"
	routeBroken = directive() + "function loop() while true do this is not lua end\n"
)

func newRouteFixture(t *testing.T) *routeFixture {
	t.Helper()
	dir := t.TempDir()
	// The shape the vision describes: the deployment's own routing, shadowing
	// the builtin by name in plugins.dir.
	file := filepath.Join(dir, "per_chat.lua")
	if err := os.WriteFile(file, []byte(routeFirst), 0o600); err != nil {
		t.Fatal(err)
	}
	builtins.SetPluginDir(dir)
	t.Cleanup(func() { builtins.SetPluginDir("") })

	src, watch, err := builtins.Resolve("plugin:per_chat")
	if err != nil {
		t.Fatal(err)
	}
	if watch != file {
		t.Fatalf("shadowed route watch path = %q, want %q", watch, file)
	}
	if src != routeFirst {
		t.Fatalf("route source is not the shadow:\n%s", src)
	}

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sup := supervisor.New(map[string]*supervisor.AgentDef{}, gateway.NewRegistry(log), pool.New(1), nil, log)

	fx := &routeFixture{file: file, sup: sup, logs: logs, log: log}
	w := New(sup, nil, log)
	w.SetRoute(&RouteSource{Ref: "plugin:per_chat", WatchPath: watch, Apply: fx.apply})
	w.Start()
	t.Cleanup(w.Stop)
	fx.w = w
	return fx
}

func (fx *routeFixture) apply(src string) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.applied = append(fx.applied, src)
}

func (fx *routeFixture) sources() []string {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return append([]string(nil), fx.applied...)
}

func (fx *routeFixture) edit(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(fx.file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, fx.file)
}

// TestRouteReloadSwapsHandler: gateway.route now has a watch entry, so an edit
// to the file behind it reaches the running router instead of waiting for a
// restart. Routing is the one piece §5.2 assigns to proprietary Lua outright
// ("gateway.route is ours"), and it was the one piece that could not be
// hot-reloaded: the watch path Resolve returns was discarded at boot.
func TestRouteReloadSwapsHandler(t *testing.T) {
	fx := newRouteFixture(t)

	// An untouched route must stay put: the mtime is seeded at Start.
	time.Sleep(1200 * time.Millisecond)
	if got := fx.sources(); len(got) != 0 {
		t.Fatalf("an untouched route was applied %d times", len(got))
	}

	fx.edit(t, routeSecond)

	waitFor(t, "the new route to be installed", 10*time.Second, func() bool {
		return len(fx.sources()) == 1
	})
	if got := fx.sources()[0]; got != routeSecond {
		t.Fatalf("applied source = %q, want the edited route", got)
	}
	if !loggedLine(fx.logs.String(), "reload: new route accepted", "file="+fx.file) {
		t.Fatalf("route reload not logged with its file:\n%s", fx.logs.String())
	}
}

// TestRouteCompileFailureKeepsRunningHandler: a broken route edit is refused
// before it is applied, so the handler serving every inbound stays the one that
// works. A typo must not take routing down for the whole deployment — the same
// guarantee a loop file gets.
func TestRouteCompileFailureKeepsRunningHandler(t *testing.T) {
	fx := newRouteFixture(t)

	fx.edit(t, routeBroken)

	waitFor(t, "the bad route to be refused", 10*time.Second, func() bool {
		return loggedLine(fx.logs.String(), "reload: route compile failed")
	})
	// Several more polls: the refusal must not turn into an accept.
	time.Sleep(1200 * time.Millisecond)

	if got := fx.sources(); len(got) != 0 {
		t.Fatalf("a route that does not compile was installed: %q", got)
	}
	if strings.Contains(fx.logs.String(), "reload: new route accepted") {
		t.Fatalf("a broken route was accepted:\n%s", fx.logs.String())
	}
}

// TestUnshadowedRouteIsNotWatched: the default route ref with no plugins.dir
// resolves to the embedded handler, which is not a file — Resolve reports no
// watch path, so there is nothing to poll and no reload to make. This is the
// one case a route edit still cannot reach, and it is the same rule a builtin
// loop follows.
func TestUnshadowedRouteIsNotWatched(t *testing.T) {
	builtins.SetPluginDir("")
	_, watch, err := builtins.Resolve("plugin:per_chat")
	if err != nil {
		t.Fatal(err)
	}
	if watch != "" {
		t.Fatalf("an embedded builtin must have no watch path, got %q", watch)
	}

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sup := supervisor.New(map[string]*supervisor.AgentDef{}, gateway.NewRegistry(log), pool.New(1), nil, log)

	applied := 0
	w := New(sup, nil, log)
	w.SetRoute(&RouteSource{Ref: "plugin:per_chat", WatchPath: watch, Apply: func(string) { applied++ }})
	w.Start()
	defer w.Stop()

	time.Sleep(1200 * time.Millisecond)
	if applied != 0 {
		t.Fatal("an unshadowed builtin route was reloaded")
	}
	if strings.Contains(logs.String(), "reload: watching route") {
		t.Fatalf("nothing should be watched for an embedded route:\n%s", logs.String())
	}
}

// supportFixture is a deployment whose plugins.dir shadows one support chunk,
// with one live session per agent. Each session's loop reports the marker the
// chunk defines, so a test can see which version of the chunk a live session is
// actually running — the thing GAP 2 was about.
type supportFixture struct {
	dir  string
	file string
	sup  *supervisor.Supervisor
	logs *syncBuf
	log  *slog.Logger
}

var (
	chunkFirst  = directive() + "SUPPORT_MARK = \"v1\"\n"
	chunkSecond = directive() + "SUPPORT_MARK = \"v2\"\n"
)

// markLoop reports the marker the shadowed chunk defines, on every turn.
const markLoop = `function loop()
  while true do
    local m = session.inbox()
    log.info("MARK:" .. tostring(SUPPORT_MARK))
  end
end
`

func newSupportFixture(t *testing.T, names ...string) *supportFixture {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "token_budget.lua")
	if err := os.WriteFile(file, []byte(chunkFirst), 0o600); err != nil {
		t.Fatal(err)
	}
	// The shadow is what makes the chunk a file on disk at all: an embedded
	// chunk has nothing to watch, which is why the deployment's plugins.dir is
	// the whole watch surface here.
	builtins.SetPluginDir(dir)
	t.Cleanup(func() { builtins.SetPluginDir("") })

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	defs := map[string]*supervisor.AgentDef{}
	for _, name := range names {
		defs[name] = &supervisor.AgentDef{
			Info:         &session.Info{Name: name, HistoryBudget: 100},
			Capabilities: map[string]bool{},
			Handlers:     map[string]session.OpHandler{},
			LoopSrc:      markLoop,
		}
	}
	sup := supervisor.New(defs, gateway.NewRegistry(log), pool.New(2), nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sup.Start(ctx)

	fx := &supportFixture{dir: dir, file: file, sup: sup, logs: logs, log: log}
	for _, name := range names {
		if err := sup.Deliver(name, "k", session.Message{ID: "m1", Type: "user", From: "u", Text: "hi"}); err != nil {
			t.Fatalf("deliver to %s: %v", name, err)
		}
	}
	// Every session must have loaded the chunk (and reported it) before the
	// test edits the file, or the edit races the first load.
	for _, name := range names {
		skey := "session=" + name + "|k"
		waitFor(t, "session "+name+" to report its chunk", 10*time.Second, func() bool {
			return loggedLine(logs.String(), "MARK:v1", skey)
		})
	}
	return fx
}

// startWatcher polls the deployment's shadowed support chunks, applying a
// change the way main wires it: every live session restarts.
func (fx *supportFixture) startWatcher(t *testing.T) *Watcher {
	t.Helper()
	w := New(fx.sup, nil, fx.log)
	w.SetSupport(&SupportSource{Paths: builtins.SupportChunkPaths(), Reload: fx.sup.ReloadAll})
	w.Start()
	t.Cleanup(w.Stop)
	return w
}

func (fx *supportFixture) edit(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(fx.file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, fx.file)
}

// TestSupportChunkEditReachesLiveSessions: a plugins.dir edit to a support
// chunk used to land only on the next new session — the chunks were re-read per
// spawn and nothing told a running one to reload. Now the watcher polls the
// shadow, and a change restarts every live session, which is where the new
// chunk is loaded. The sessions are all of them: every session loads every
// support chunk, so there is no per-agent reference to key a reload by.
func TestSupportChunkEditReachesLiveSessions(t *testing.T) {
	fx := newSupportFixture(t, "alpha", "beta")
	fx.startWatcher(t)

	fx.edit(t, chunkSecond)

	waitFor(t, "the change to be applied", 10*time.Second, func() bool {
		return loggedLine(fx.logs.String(), "reload: support chunks updated", "chunks=token_budget")
	})
	waitFor(t, "both sessions to restart", 10*time.Second, func() bool {
		return len(agentsNamed(fx.logs.String(), "hot reload: restarting loop", "session=", "alpha", "beta")) == 2
	})

	// The restarted sessions must be running the new chunk, not merely
	// restarted: a turn now reports v2.
	for _, name := range []string{"alpha", "beta"} {
		if err := fx.sup.Deliver(name, "k", session.Message{ID: "m2", Type: "user", From: "u", Text: "again"}); err != nil {
			t.Fatalf("deliver to %s: %v", name, err)
		}
	}
	for _, name := range []string{"alpha", "beta"} {
		skey := "session=" + name + "|k"
		waitFor(t, "session "+name+" to run the new chunk", 10*time.Second, func() bool {
			return loggedLine(fx.logs.String(), "MARK:v2", skey)
		})
	}
}

// TestSupportChunkCompileFailureKeepsOldVersion: a broken chunk edit is refused
// before any session is restarted, so live sessions keep running the version
// already loaded — a typo in a support chunk cannot take sessions down.
func TestSupportChunkCompileFailureKeepsOldVersion(t *testing.T) {
	fx := newSupportFixture(t, "alpha")
	fx.startWatcher(t)

	fx.edit(t, "SUPPORT_MARK = \"v2\"\nfunction broken( end\n")

	waitFor(t, "the bad chunk to be refused", 10*time.Second, func() bool {
		return loggedLine(fx.logs.String(), "reload: support chunk compile failed", "chunk=token_budget")
	})
	time.Sleep(1200 * time.Millisecond)

	if strings.Contains(fx.logs.String(), "reload: support chunks updated") {
		t.Fatalf("a chunk that does not compile was applied:\n%s", fx.logs.String())
	}
	if strings.Contains(fx.logs.String(), "hot reload: restarting loop") {
		t.Fatalf("sessions were restarted for a broken chunk:\n%s", fx.logs.String())
	}

	// The live session is still the one that works.
	if err := fx.sup.Deliver("alpha", "k", session.Message{ID: "m2", Type: "user", From: "u", Text: "again"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the session to keep running the old chunk", 10*time.Second, func() bool {
		return countLogged(fx.logs.String(), "MARK:v1") >= 2
	})
}

// TestUnchangedSupportChunkDoesNotReload: an untouched shadow stays quiet
// across polls — the mtime is seeded at Start, so only a real edit restarts the
// deployment's sessions.
func TestUnchangedSupportChunkDoesNotReload(t *testing.T) {
	fx := newSupportFixture(t, "alpha")
	fx.startWatcher(t)

	time.Sleep(1600 * time.Millisecond)

	if strings.Contains(fx.logs.String(), "reload: support chunks updated") {
		t.Fatalf("an untouched support chunk must not reload:\n%s", fx.logs.String())
	}
}
