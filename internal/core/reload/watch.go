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

// Watcher polls file mtimes (no fsnotify dependency).
type Watcher struct {
	sup    *supervisor.Supervisor
	log    *slog.Logger
	mtimes map[string]time.Time
	stop   chan struct{}
}

func New(sup *supervisor.Supervisor, log *slog.Logger) *Watcher {
	return &Watcher{
		sup:    sup,
		log:    log.With("module", "reload"),
		mtimes: map[string]time.Time{},
		stop:   make(chan struct{}),
	}
}

func (w *Watcher) Start() {
	for _, def := range w.sup.Agents() {
		for _, p := range []string{def.LoopFile, def.InstructionsPath} {
			if p == "" {
				continue
			}
			if fi, err := os.Stat(p); err == nil {
				w.mtimes[p] = fi.ModTime()
				if fi.IsDir() {
					w.seedDir(p)
				}
			}
		}
	}
	go w.loop()
}

// seedDir records the mtimes of a directory loop's members so the first
// member edit after startup is detected.
func (w *Watcher) seedDir(dir string) {
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
			w.mtimes[p] = fi.ModTime()
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
		if def.LoopFile != "" && w.changed(def.LoopFile) {
			w.reloadLoop(name, def.LoopFile)
		}
		if def.InstructionsPath != "" && w.changed(def.InstructionsPath) {
			w.reloadInstructions(name, def)
		}
	}
}

// changed reports whether the path's mtime advanced since the last poll and
// records the new mtime. For a directory loop, it reports whether any member
// *.lua changed (the directory's own mtime is unreliable for member edits).
func (w *Watcher) changed(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !fi.IsDir() {
		if fi.ModTime().Equal(w.mtimes[path]) {
			return false
		}
		w.mtimes[path] = fi.ModTime()
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
		if !mfi.ModTime().Equal(w.mtimes[p]) {
			w.mtimes[p] = mfi.ModTime()
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
