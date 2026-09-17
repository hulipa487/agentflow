// Package reload watches loop plugin files, instructions files, and
// file-backed prompt registry entries. A loop file that fails to compile keeps
// the old version running — a typo never kills a live agent. Instructions are
// markdown: they hot-update the shared per-agent content, and loops pick them
// up on the next turn (no session restart). A loop may be a directory: each
// member *.lua is watched and an edit to any one re-resolves the whole loop
// (members concatenate in sorted order).
//
// Prompts are the deployment-wide registry (config `prompts:`). Only the
// *contents* of a `file:`-backed entry are live: adding a key, deleting one,
// re-pointing a key at a different file, or switching an entry between
// `inline:` and `file:` all change the config, and that still needs a restart.
// `inline:`/`text:` entries have no file to poll and are never watched. Unlike
// the per-(agent, path) loop bookkeeping, prompt files are tracked once for
// the whole deployment: the registry is shared by every agent by design, so a
// single edit updates all of them — keying that by agent would let whichever
// agent poll() reached first consume the change and leave the rest stale.
package reload

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentflow/internal/core/session"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/vm"
)

// PromptSource is the deployment's shared prompt registry plus the file
// backing each `file:`-sourced key. Keys with an inline/text source are absent
// — there is nothing to watch. Files is keyed by prompt name.
type PromptSource struct {
	Registry *session.PromptRegistry
	Files    map[string]string
}

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
	sup     *supervisor.Supervisor
	prompts *PromptSource
	log     *slog.Logger
	mtimes  map[watchKey]time.Time
	// members is the set of *.lua member paths last seen for a directory loop,
	// per (agent, dir). A member's mtime cannot report its own disappearance —
	// it is simply absent from the next readdir — so the set is what makes a
	// deleted member a change.
	members map[watchKey]map[string]bool
	// promptMtimes and promptBad are keyed by prompt name (not by agent): the
	// registry is one deployment-wide resource, so a file is polled once and
	// the update fans out to every agent that reads it.
	promptMtimes map[string]time.Time
	promptBad    map[string]bool
	stop         chan struct{}
}

func New(sup *supervisor.Supervisor, prompts *PromptSource, log *slog.Logger) *Watcher {
	return &Watcher{
		sup:          sup,
		prompts:      prompts,
		log:          log.With("module", "reload"),
		mtimes:       map[watchKey]time.Time{},
		members:      map[watchKey]map[string]bool{},
		promptMtimes: map[string]time.Time{},
		promptBad:    map[string]bool{},
		stop:         make(chan struct{}),
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
	w.seedPrompts()
	go w.loop()
}

// seedPrompts records the current mtime of every file-backed prompt, so the
// first edit after startup is what fires — an unchanged file is not a change.
func (w *Watcher) seedPrompts() {
	if w.prompts == nil {
		return
	}
	for key, path := range w.prompts.Files {
		if fi, err := os.Stat(path); err == nil {
			w.promptMtimes[key] = fi.ModTime()
		}
	}
}

// seedDir records the mtimes and the member set of a directory loop for one
// agent, so the first member edit after startup is detected — and so a member
// deleted later is compared against what was there at startup.
func (w *Watcher) seedDir(agent, dir string) {
	names, err := dirMembers(dir)
	if err != nil {
		return
	}
	members := map[string]bool{}
	for _, n := range names {
		p := filepath.Join(dir, n)
		if fi, err := os.Stat(p); err == nil {
			w.mtimes[watchKey{agent, p}] = fi.ModTime()
			members[p] = true
		}
	}
	w.members[watchKey{agent, dir}] = members
}

// dirMembers lists a directory's *.lua member names, sorted (the concatenation
// order builtins.Resolve uses).
func dirMembers(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
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
	// Prompts are polled once per tick for the whole deployment, not per
	// agent: one shared registry means one change fans out to everyone.
	w.pollPrompts()
	for name, def := range w.sup.Agents() {
		if def.LoopFile != "" && w.changed(name, def.LoopFile) {
			w.reloadLoop(name, def.LoopFile)
		}
		if def.InstructionsPath != "" && w.changed(name, def.InstructionsPath) {
			w.reloadInstructions(name, def)
		}
	}
}

// pollPrompts re-reads every file-backed prompt whose mtime advanced and
// republishes it to the whole deployment. A file that cannot be read keeps its
// previous text (never an empty prompt) and warns once, not on every tick.
func (w *Watcher) pollPrompts() {
	if w.prompts == nil || w.prompts.Registry == nil {
		return
	}
	for key, path := range w.prompts.Files {
		fi, err := os.Stat(path)
		if err != nil {
			w.promptUnreadable(key, path, err)
			continue
		}
		// Unchanged, and not mid-recovery from a failed read.
		if fi.ModTime().Equal(w.promptMtimes[key]) && !w.promptBad[key] {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			w.promptUnreadable(key, path, err)
			continue
		}
		w.promptMtimes[key] = fi.ModTime()
		delete(w.promptBad, key)
		text := string(b)
		w.prompts.Registry.Set(key, text)
		w.log.Info("reload: prompt updated", "prompt", key, "file", path)
		w.updatePromptInstructions(key, text)
	}
}

// promptUnreadable warns that a prompt file could not be re-read. The warning
// is emitted on the transition into the unreadable state only: the mtime is
// deliberately left alone so the next tick retries, and warning every retry
// would flood the log at the poll interval.
func (w *Watcher) promptUnreadable(key, path string, err error) {
	if w.promptBad[key] {
		return
	}
	w.promptBad[key] = true
	w.log.Warn("reload: cannot read prompt, keeping current text",
		"prompt", key, "file", path, "err", err)
}

// updatePromptInstructions refreshes the system prompt of every agent whose
// instructions came from this prompt key. It updates in place, exactly as
// reloadInstructions does — the session is not restarted, so the loop picks
// the new text up on its next turn.
func (w *Watcher) updatePromptInstructions(key, text string) {
	for name, def := range w.sup.Agents() {
		if def.Info == nil || def.Info.InstructionsPrompt != key {
			continue
		}
		if def.Info.Instructions == nil {
			continue
		}
		def.Info.Instructions.Store(text)
		w.log.Info("reload: instructions updated", "prompt", key, "agent", name)
	}
}

// changed reports whether the path's mtime advanced since this agent last
// looked and records the new mtime. For a directory loop, it reports whether
// any member *.lua changed — an edit, an addition, or a deletion (the
// directory's own mtime is unreliable for member edits). The bookkeeping is per
// (agent, path): a path shared by several agents is tracked separately for each
// of them, so one agent's poll cannot consume another's change.
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
	names, err := dirMembers(path)
	if err != nil {
		// A transient readdir failure must not look like every member vanishing.
		return false
	}
	seen := make(map[string]bool, len(names))
	var changed bool
	for _, n := range names {
		p := filepath.Join(path, n)
		seen[p] = true
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
	// A member that is gone is a change too — the concatenated loop source
	// shrank, and no mtime on disk can report that (a deleted file is simply
	// absent from the listing above). Drop its entry in the same pass so
	// removed members do not accumulate.
	for p := range w.members[key] {
		if !seen[p] {
			delete(w.mtimes, watchKey{agent, p})
			changed = true
		}
	}
	w.members[key] = seen
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
// in sorted order (matching builtins.Resolve). An empty directory is an error,
// not an empty loop: builtins.Resolve — which the actor calls on every restart
// — rejects that same condition, so accepting it here would install a loop the
// runtime cannot load, and the session would crash-restart once a second.
func readLoop(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		b, err := os.ReadFile(path)
		return string(b), err
	}
	names, err := dirMembers(path)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("loop dir %s has no .lua files", path)
	}
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
