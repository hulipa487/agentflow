// Package reload watches loop plugin files and instructions files.
// A loop file that fails to compile keeps the old version running — a typo
// never kills a live agent. Instructions are markdown: they hot-update the
// shared per-agent content, and loops pick them up on the next turn (no
// session restart).
package reload

import (
	"log/slog"
	"os"
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
			}
		}
	}
	go w.loop()
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

// changed reports whether the file's mtime advanced since the last poll and
// records the new mtime.
func (w *Watcher) changed(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if fi.ModTime().Equal(w.mtimes[path]) {
		return false
	}
	w.mtimes[path] = fi.ModTime()
	return true
}

func (w *Watcher) reloadLoop(agent, path string) {
	src, err := os.ReadFile(path)
	if err != nil {
		w.log.Error("reload: cannot read file", "file", path, "err", err)
		return
	}
	if err := vm.CompileCheck("@"+path, string(src)); err != nil {
		w.log.Error("reload: compile failed, keeping old version", "file", path, "err", err)
		return
	}
	w.log.Info("reload: new version accepted, restarting sessions", "file", path, "agent", agent)
	w.sup.ReloadAgent(agent)
}

func (w *Watcher) reloadInstructions(agent string, def *supervisor.AgentDef) {
	b, err := os.ReadFile(def.InstructionsPath)
	if err != nil {
		w.log.Error("reload: cannot read instructions", "file", def.InstructionsPath, "err", err)
		return
	}
	def.Info.Instructions.Store(string(b))
	w.log.Info("reload: instructions updated", "file", def.InstructionsPath, "agent", agent)
}
