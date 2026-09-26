// Package reload watches the deployment's Lua policy: loop plugin files,
// instructions files, file-backed prompt registry entries, the gateway route
// handler, and the plugins.dir shadows of the support chunks. A file that fails
// to compile keeps the old version running — a typo never kills a live agent or
// takes routing down. Instructions and prompts hot-update the shared content,
// and loops pick them up on the next turn (no session restart). A loop may be a
// directory: each member *.lua is watched and an edit to any one re-resolves the
// whole loop (members concatenate in sorted order).
//
// The same "keep the old version" rule covers a loop whose prelude API version
// no longer matches this core (vm.ErrPreludeVersion): the file on disk is
// refused, at Error rather than Warn, and the running loop — which is still
// matched to this core — keeps serving. Note what that does and does not buy.
// It buys a hot reload that cannot install a chunk against the wrong API. It
// does not buy a running deployment any protection from a *core* upgrade: a new
// binary loads every loop fresh, and a loop left at the old version refuses to
// load there, which is the loud failure the version gate exists to produce. And
// it does not make the bad file safe to leave lying around: a session that
// restarts for any reason re-reads its loop from disk through
// builtins.Resolve, so it refuses and retries too. The file is what has to be
// fixed — the refusal is a message, not a repair.
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
//
// The route and the support chunks are policy too, and are watched on the same
// terms as a loop: the route is swapped by rebuilding the router's service
// state, and a support chunk restarts every live session (it is loaded into
// each of them). A ref with no file behind it — an unshadowed `plugin:` — has
// nothing to poll and is never watched, exactly like a builtin loop.
package reload

import (
	"errors"
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

// RouteSource is the gateway's route handler as the watcher sees it: the
// reference the deployment resolved (config `gateway.route`) and the file
// behind it. Ref is for logs; WatchPath is "" when the ref resolves to an
// unshadowed builtin, which has nothing on disk to poll.
type RouteSource struct {
	Ref       string
	WatchPath string
	// Apply installs a new route source. It is called from the watcher's poll
	// goroutine, after the source compiles, and must not block on the router:
	// Router.Reload signals a running state rather than waiting for it.
	Apply func(src string)
}

// SupportSource is the deployment's shadowed support chunks: chunk name → the
// plugins.dir file overriding it, plus how to apply a change. Every session
// loads all of them before its loop, so there is no per-agent reference to key
// a reload by — Reload restarts every live session, each at its next safe
// point, and the restart is where the new chunk is loaded.
type SupportSource struct {
	Paths  map[string]string
	Reload func()
}

// routeOwner and supportOwner are the watchKey owners for the two pieces of
// policy that run outside a session, and so have no agent name to be keyed by.
// Both start with NUL, which a config agent name (a YAML key) cannot contain:
// without that, a route path or a chunk that an agent happens to share would
// share an mtime entry, and whichever poll reached it first would consume the
// change.
const (
	routeOwner   = "\x00route"
	supportOwner = "\x00support:"
)

// watchKey identifies one watched path for one owner. Several agents may share
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
	route   *RouteSource
	support *SupportSource
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

// SetRoute registers the gateway route handler for polling. Call before Start:
// Start seeds the watch path's mtime, so an edit made before Start is not
// mistaken for one that happened after it.
func (w *Watcher) SetRoute(r *RouteSource) { w.route = r }

// SetSupport registers the plugins.dir support-chunk shadows for polling. Call
// before Start, for the same reason as SetRoute.
func (w *Watcher) SetSupport(s *SupportSource) { w.support = s }

func (w *Watcher) Start() {
	for name, def := range w.sup.Agents() {
		for _, p := range []string{def.LoopFile, def.InstructionsPath} {
			if p != "" {
				w.seedPath(name, p)
			}
		}
	}
	if w.route != nil && w.route.WatchPath != "" {
		w.seedPath(routeOwner, w.route.WatchPath)
		w.log.Info("reload: watching route", "route", w.route.Ref, "file", w.route.WatchPath)
	}
	for name, path := range w.supportPaths() {
		w.seedPath(supportOwner+name, path)
		w.log.Info("reload: watching support chunk", "chunk", name, "file", path)
	}
	w.seedPrompts()
	go w.loop()
}

// supportPaths is the registered support-chunk watch list, nil when none is
// registered or nothing is shadowed.
func (w *Watcher) supportPaths() map[string]string {
	if w.support == nil {
		return nil
	}
	return w.support.Paths
}

// seedPath records the current mtime of one watched path, and the member set
// when it is a directory loop, so the first edit after startup is what fires —
// an unchanged file is not a change.
func (w *Watcher) seedPath(owner, path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	w.mtimes[watchKey{owner, path}] = fi.ModTime()
	if fi.IsDir() {
		w.seedDir(owner, path)
	}
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
// owner, so the first member edit after startup is detected — and so a member
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
	// The route and the support chunks are deployment-wide too: one route
	// serves every inbound, and every session loads every support chunk.
	w.pollRoute()
	w.pollSupport()
	for name, def := range w.sup.Agents() {
		if def.LoopFile != "" && w.changed(name, def.LoopFile) {
			w.reloadLoop(name, def.LoopFile)
		}
		if def.InstructionsPath != "" && w.changed(name, def.InstructionsPath) {
			w.reloadInstructions(name, def)
		}
	}
}

// pollRoute re-reads the route handler when its file changes and swaps it in.
// The route runs in the router's service state rather than in a session, so
// there is no session to restart: the new source compiles and the router
// rebuilds its state from it. A route that does not compile is refused here and
// the running handler stays in place — a broken edit must not take routing
// down. Nothing is watched when the ref is an unshadowed builtin (WatchPath
// ""); the embedded handler is not a file.
func (w *Watcher) pollRoute() {
	if w.route == nil || w.route.WatchPath == "" || w.route.Apply == nil {
		return
	}
	if !w.changed(routeOwner, w.route.WatchPath) {
		return
	}
	src, err := readLoop(w.route.WatchPath)
	if err != nil {
		w.log.Warn("reload: cannot read route", "route", w.route.Ref, "file", w.route.WatchPath, "err", err)
		return
	}
	if err := vm.CompileCheck("@"+w.route.WatchPath, src); err != nil {
		w.log.Warn("reload: route compile failed, keeping old version",
			"route", w.route.Ref, "file", w.route.WatchPath, "err", err)
		return
	}
	w.log.Info("reload: new route accepted, rebuilding the route state",
		"route", w.route.Ref, "file", w.route.WatchPath)
	w.route.Apply(src)
}

// pollSupport re-reads each shadowed support chunk whose file changed and, if
// they compile, restarts every live session — the chunks are loaded into each
// session's Lua state before its loop, so a session must restart to take a new
// one. A chunk that does not compile is refused and sessions keep running the
// version already loaded; only a later edit is retried. The refusal protects
// what is running, not what starts next: a session spawned while the bad edit
// is on disk still reads the file, as it always has.
func (w *Watcher) pollSupport() {
	if w.support == nil {
		return
	}
	changed := w.supportChanged()
	if len(changed) == 0 {
		return
	}
	w.log.Info("reload: support chunks updated, restarting sessions",
		"chunks", strings.Join(changed, ","))
	if w.support.Reload != nil {
		w.support.Reload()
	}
}

// supportChanged reports which registered support-chunk shadows changed and
// compile, sorted by name. Each one is checked on its own: one bad edit must
// not block the others, and a chunk that fails to compile leaves the sessions
// running what they already have.
func (w *Watcher) supportChanged() []string {
	var changed []string
	for name, path := range w.supportPaths() {
		if !w.changed(supportOwner+name, path) {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			w.log.Warn("reload: cannot read support chunk", "chunk", name, "file", path, "err", err)
			continue
		}
		if err := vm.CheckChunkVersion(path, string(b)); err != nil {
			w.log.Error("reload: support chunk version mismatch, keeping old version",
				"chunk", name, "file", path, "err", err)
			continue
		}
		if err := vm.CompileCheck("@"+path, string(b)); err != nil {
			w.log.Warn("reload: support chunk compile failed, keeping old version",
				"chunk", name, "file", path, "err", err)
			continue
		}
		changed = append(changed, name)
	}
	sort.Strings(changed)
	return changed
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

// changed reports whether the path's mtime advanced since this owner last
// looked and records the new mtime. For a directory loop, it reports whether
// any member *.lua changed — an edit, an addition, or a deletion (the
// directory's own mtime is unreliable for member edits). The bookkeeping is per
// (owner, path), where an owner is an agent name or one of the two
// outside-a-session sentinels above: a path shared by several owners is tracked
// separately for each of them, so one owner's poll cannot consume another's
// change.
//
// A (owner, path) pair never seen before is seeded, not reported: it arrives
// when an agent was registered after Start — a runtime upsert — so there is
// no previous state to have changed from. Boot-time agents were all seeded in
// Start, so this only fires for the registry's additions; without it, the
// zero-value mtime would read as a change and every live session of a
// just-upserted agent would take a pointless restart.
func (w *Watcher) changed(agent, path string) bool {
	key := watchKey{agent, path}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !fi.IsDir() {
		prev, seen := w.mtimes[key]
		mtime := fi.ModTime()
		w.mtimes[key] = mtime
		return seen && !mtime.Equal(prev)
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
		// A version refusal is not a typo to be retried: the file on disk
		// targets a prelude this core does not provide, and loading it would
		// run the chunk against an API it was not written for. Refusing keeps
		// the running version — which is still matched to this core — and the
		// operator gets the file, the version it declares and the version this
		// core provides, at Error rather than Warn, because nothing about this
		// situation is routine.
		if errors.Is(err, vm.ErrPreludeVersion) {
			w.log.Error("reload: prelude version mismatch, keeping old version", "file", path, "err", err)
			return
		}
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

// readLoop reads a plugin source path — a loop's or the route's, which resolve
// the same way — concatenating a directory's *.lua members in sorted order
// (matching builtins.Resolve). An empty directory is an error, not an empty
// plugin: builtins.Resolve — which the actor calls on every restart — rejects
// that same condition, so accepting it here would install a plugin the runtime
// cannot load, and the session would crash-restart once a second.
//
// Every member read here must also pass the prelude version gate, for the same
// reason: builtins.Resolve enforces it on the actor's path, so a plugin the
// watcher accepted and Resolve refused would be a plugin that passes hot reload
// and then fails on the next session restart. Members are checked one by one
// rather than on the concatenation, because only the first member's directive
// leads the joined source.
func readLoop(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if err := vm.CheckChunkVersion(path, string(b)); err != nil {
			return "", err
		}
		return string(b), nil
	}
	names, err := dirMembers(path)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("plugin dir %s has no .lua files", path)
	}
	var parts []string
	for _, n := range names {
		p := filepath.Join(path, n)
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		if err := vm.CheckChunkVersion(p, string(b)); err != nil {
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
