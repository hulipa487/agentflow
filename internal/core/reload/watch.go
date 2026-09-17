// Package reload watches loop plugin files and instructions files.
// A loop file that fails to compile keeps the old version running — a typo
// never kills a live agent. Instructions are markdown: they hot-update the
// shared per-agent content, and loops pick them up on the next turn (no
// session restart). A loop may be a directory: each member *.lua is watched
// and an edit to any one re-resolves the whole loop (members concatenate in
// sorted order).
package reload

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentflow/internal/core/supervisor"
	"agentflow/internal/vm"
)

// watchKey identifies one watched path for one agent. Several agents may share
// a loop path — a supported and useful shape, e.g. one shared loop whose Lua
// tools each profile narrows via its skills list — and every one of them has
// to be reloaded when that path changes. Bookkeeping keyed by path alone would
// let the first agent polled consume the change (it is the one that records the
// new mtime) and leave every other agent bound to that path stale, so an edit
// would appear half-applied depending on map iteration order.
type watchKey struct {
	agent string
	path  string
}

// Watcher polls file mtimes (no fsnotify dependency).
type Watcher struct {
	sup    *supervisor.Supervisor
	log    *slog.Logger
	mtimes map[watchKey]time.Time
	stop   chan struct{}
}

func New(sup *supervisor.Supervisor, log *slog.Logger) *Watcher {
	return &Watcher{
		sup:    sup,
		log:    log.With("module", "reload"),
		mtimes: map[watchKey]time.Time{},
		stop:   make(chan struct{}),
	}
}

func (w *Watcher) Start() {
	for name, def := range w.sup.Agents() {
		for _, p := range []string{def.LoopFile, def.InstructionsPath} {
			if p == "" {
				continue
			}
			if fi, err := os.Stat(p); err == nil {
				w.mtimes[watchKey{name, p}] = fi.ModTime()
				if fi.IsDir() {
					w.seedDir(name, p)
				}
			}
		}
	}
	go w.loop()
}

// seedDir records the mtimes of a directory loop's members for one agent, so
// the first member edit after startup is detected.
func (w *Watcher) seedDir(agent, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if fi, err := os.Stat(p); err == nil {
			w.mtimes[watchKey{agent, p}] = fi.ModTime()
		}
	}
}

func (w *Watcher) Stop() { close(w.stop) }

func (w *Watcher) loop() {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-tick.C:
			w.poll()
		}
	}
}

func (w *Watcher) poll() {
	for name, def := range w.sup.Agents() {
		if def.LoopFile != "" && w.changed(name, def.LoopFile) {
			w.reloadLoop(name, def.LoopFile)
		}
		if def.InstructionsPath != "" && w.changed(name, def.InstructionsPath) {
			w.reloadInstructions(name, def)
		}
	}
}

// changed reports whether the path's mtime advanced since this agent last
// looked and records the new mtime. For a directory loop, it reports whether
// any member *.lua changed (the directory's own mtime is unreliable for member
// edits). The bookkeeping is per (agent, path): a path shared by several agents
// is tracked separately for each of them, so one agent's poll cannot consume
// another's change.
func (w *Watcher) changed(agent, path string) bool {
	key := watchKey{agent, path}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !fi.IsDir() {
		if fi.ModTime().Equal(w.mtimes[key]) {
			return false
		}
		w.mtimes[key] = fi.ModTime()
		return true
	}
	// Directory: check each member's mtime; also re-seed so newly added
	// members are tracked on subsequent polls.
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	var changed bool
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		p := filepath.Join(path, e.Name())
		mfi, err := os.Stat(p)
		if err != nil {
			continue
		}
		mk := watchKey{agent, p}
		if !mfi.ModTime().Equal(w.mtimes[mk]) {
			w.mtimes[mk] = mfi.ModTime()
			changed = true
		}
	}
	return changed
}

func (w *Watcher) reloadLoop(agent, path string) {
	src, err := readLoop(path)
	if err != nil {
		w.log.Warn("reload: cannot read loop", "file", path, "err", err)
		return
	}
	if err := vm.CompileCheck("@"+path, src); err != nil {
		w.log.Warn("reload: compile failed, keeping old version", "file", path, "err", err)
		return
	}
	w.log.Info("reload: new version accepted, restarting sessions", "file", path, "agent", agent)
	w.sup.ReloadAgent(agent)
}

// readLoop reads a loop source path, concatenating a directory's *.lua members
// in sorted order (matching builtins.Resolve).
func readLoop(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		b, err := os.ReadFile(path)
		return string(b), err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".lua") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var parts []string
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(path, n))
		if err != nil {
			return "", err
		}
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "\n"), nil
}

func (w *Watcher) reloadInstructions(agent string, def *supervisor.AgentDef) {
	b, err := os.ReadFile(def.InstructionsPath)
	if err != nil {
		w.log.Warn("reload: cannot read instructions", "file", def.InstructionsPath, "err", err)
		return
	}
	def.Info.Instructions.Store(string(b))
	w.log.Info("reload: instructions updated", "file", def.InstructionsPath, "agent", agent)
}
