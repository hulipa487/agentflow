package studio

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"agentflow/internal/config"
)

// ErrMemoryConflict means the memory source changed since the caller read it.
var ErrMemoryConflict = errors.New("the memory configuration changed on disk since it was read; reload before saving")

// MemoryStoreDraft is one logical store of a memory profile.
type MemoryStoreDraft struct {
	Name       string   `json:"name"`
	Backend    string   `json:"backend"`
	Table      string   `json:"table,omitempty"`
	Retention  string   `json:"retention,omitempty"`
	Window     int      `json:"window,omitempty"`
	Requires   []string `json:"requires,omitempty"`
	Scope      string   `json:"scope,omitempty"`
	Pool       string   `json:"pool,omitempty"`
	LocalityOK bool     `json:"locality_ok,omitempty"`
}

// MemoryProfileDraft is the editable shape of one profiles.memory entry.
// Stores keep the order the editor shows them in; the file stores a map.
type MemoryProfileDraft struct {
	Name        string             `json:"name"`
	Stores      []MemoryStoreDraft `json:"stores"`
	Write       []string           `json:"write,omitempty"`
	Recall      string             `json:"recall,omitempty"`
	EmbedModel  string             `json:"embed_model,omitempty"`
	RerankModel string             `json:"rerank_model,omitempty"`
	Oversample  int                `json:"oversample,omitempty"`
}

// MemoryBackendDraft is the editable shape of one memory.backends entry. The
// URL and API key are write only, like a channel's token: an empty value keeps
// the stored one and the Clear flags remove it. Keys the editor does not know
// (a password, say) are kept as they are.
type MemoryBackendDraft struct {
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	Path            string `json:"path,omitempty"`
	URL             string `json:"url,omitempty"`
	ClearURL        bool   `json:"clear_url,omitempty"`
	APIKey          string `json:"api_key,omitempty"`
	ClearAPIKey     bool   `json:"clear_api_key,omitempty"`
	Collection      string `json:"collection,omitempty"`
	Distance        string `json:"distance,omitempty"`
	Timeout         string `json:"timeout,omitempty"`
	Dim             int    `json:"dim,omitempty"`
	VectorScanLimit int    `json:"vector_scan_limit,omitempty"`
	MaxEntries      int    `json:"max_entries,omitempty"`
}

// MemoryBackendView is a backend as the editor sees it. Secrets never leave
// the runtime: Has* says one is stored, *Ref carries an env or credential
// reference verbatim, and URLDisplay is the URL with any password masked.
type MemoryBackendView struct {
	Name            string   `json:"name"`
	Provider        string   `json:"provider"`
	Path            string   `json:"path,omitempty"`
	Collection      string   `json:"collection,omitempty"`
	Distance        string   `json:"distance,omitempty"`
	Timeout         string   `json:"timeout,omitempty"`
	Dim             int      `json:"dim,omitempty"`
	VectorScanLimit int      `json:"vector_scan_limit,omitempty"`
	MaxEntries      int      `json:"max_entries,omitempty"`
	HasURL          bool     `json:"has_url"`
	URLRef          string   `json:"url_ref,omitempty"`
	URLDisplay      string   `json:"url_display,omitempty"`
	HasAPIKey       bool     `json:"has_api_key"`
	APIKeyRef       string   `json:"api_key_ref,omitempty"`
	OtherKeys       []string `json:"other_keys"`
	Profiles        []string `json:"profiles"`
	Agents          []string `json:"agents"`
	// Implicit marks the main_db backend a config with no backends gets on the
	// runtime store. It is not in the file; saving any backend writes it out.
	Implicit bool `json:"implicit"`
	Live     bool `json:"live"`
}

// MemoryProfileView is one profile plus the agents and spawn templates that
// name it, which decide whether a delete is allowed.
type MemoryProfileView struct {
	MemoryProfileDraft
	Agents        []string `json:"agents"`
	SpawnProfiles []string `json:"spawn_profiles"`
	Live          bool     `json:"live"`
}

// MemoryList is the whole editable memory surface and the names an editor
// may offer.
type MemoryList struct {
	Mode          string              `json:"mode"` // configdir | file
	Path          string              `json:"path,omitempty"`
	Editable      bool                `json:"editable"`
	MTime         int64               `json:"mtime,omitempty"`
	Providers     []string            `json:"providers"`
	Backends      []MemoryBackendView `json:"backends"`
	Profiles      []MemoryProfileView `json:"profiles"`
	BuiltinAgents []string            `json:"builtin_agents"`
	Models        []string            `json:"models"`
	RerankModels  []string            `json:"rerank_models"`
	WritePlugins  []string            `json:"write_plugins"`
	RecallPlugins []string            `json:"recall_plugins"`
	Requires      []string            `json:"requires"`
	Note          string              `json:"note"`
}

// The fixed vocabularies the editor offers.
var (
	memoryWritePlugins  = []string{"plugin:routing_table", "plugin:fact_extractor"}
	memoryRecallPlugins = []string{"plugin:recency", "plugin:semantic"}
	memoryRequires      = []string{"kv", "text_search", "vector"}
	memoryScopes        = []string{"", "user", "agent"}
)

// backendKeys is which config keys each provider reads. Keys a provider does
// not read are dropped when a backend changes provider.
var backendKeys = map[string][]string{
	"sqlite":      {"path", "vector_scan_limit"},
	"postgres":    {"url", "dim"},
	"pgvector":    {"url", "dim"},
	"redis":       {"url"},
	"redisvector": {"url", "dim"},
	"qdrant":      {"url", "api_key", "collection", "dim", "distance", "timeout"},
	"volatile":    {"max_entries"},
}

// managedBackendKeys is every key the editor sets; any other key is kept.
var managedBackendKeys = map[string]bool{
	"path": true, "url": true, "api_key": true, "collection": true, "distance": true,
	"timeout": true, "dim": true, "vector_scan_limit": true, "max_entries": true,
}

// Memory persists memory backends and memory profiles. Agents open their
// stores at boot, so a save is durable but not live: the API reports
// restart_required.
type Memory struct {
	mu         sync.Mutex
	cfg        *config.Config
	configDir  string
	configPath string
	log        *slog.Logger
}

// NewMemory builds the editor over the deployment's config source.
func NewMemory(cfg *config.Config, configDir, configPath string, log *slog.Logger) *Memory {
	if log == nil {
		log = slog.Default()
	}
	if abs, err := filepath.Abs(configDir); err == nil && configDir != "" {
		configDir = abs
	}
	if abs, err := filepath.Abs(configPath); err == nil && configPath != "" {
		configPath = abs
	}
	return &Memory{cfg: cfg, configDir: configDir, configPath: configPath, log: log}
}

// memorySections is the subset of the config this editor owns, as written in
// the file: references are unexpanded and paths are not rebased.
type memorySections struct {
	backends map[string]config.Backend
	profiles map[string]config.MemoryProfile
	// persistence is runtime.persistence as written, for writing out the
	// implicit main_db.
	persistence string
}

func (s memorySections) clone() memorySections {
	out := memorySections{
		backends:    make(map[string]config.Backend, len(s.backends)),
		profiles:    make(map[string]config.MemoryProfile, len(s.profiles)),
		persistence: s.persistence,
	}
	for k, v := range s.backends {
		c := make(map[string]any, len(v.Config))
		for ck, cv := range v.Config {
			c[ck] = cv
		}
		out.backends[k] = config.Backend{Provider: v.Provider, Config: c}
	}
	for k, v := range s.profiles {
		out.profiles[k] = v
	}
	return out
}

func (m *Memory) sourcePath() string {
	if m.configDir != "" {
		return filepath.Join(m.configDir, "system.yaml")
	}
	return m.configPath
}

func (m *Memory) boot() *config.Config {
	if m.cfg == nil {
		return &config.Config{}
	}
	return m.cfg
}

// diskConfig loads the source as it is on disk, so validation and usage see
// what a restart would.
func (m *Memory) diskConfig() (*config.Config, error) {
	if m.configDir != "" {
		return config.LoadDir(m.configDir, m.log)
	}
	if m.configPath == "" {
		return m.cfg, nil
	}
	return config.Load(m.configPath)
}

// current is the config usage is computed from: the disk config when it
// loads, the boot snapshot otherwise.
func (m *Memory) current() *config.Config {
	if disk, err := m.diskConfig(); err == nil && disk != nil {
		return disk
	}
	return m.boot()
}

// List reads the persisted memory surface.
func (m *Memory) List() (MemoryList, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listLocked()
}

func (m *Memory) listLocked() (MemoryList, error) {
	path := m.sourcePath()
	cur := m.current()
	out := MemoryList{
		Mode:          "file",
		Path:          path,
		Editable:      path != "",
		Providers:     config.MemoryProviders(),
		Backends:      []MemoryBackendView{},
		Profiles:      []MemoryProfileView{},
		BuiltinAgents: []string{},
		Models:        modelNames(cur, ""),
		RerankModels:  modelNames(cur, "rerank"),
		WritePlugins:  append([]string(nil), memoryWritePlugins...),
		RecallPlugins: append([]string(nil), memoryRecallPlugins...),
		Requires:      append([]string(nil), memoryRequires...),
		Note:          "Agents open their memory stores when agentflow starts; saved changes take effect after a restart.",
	}
	if m.configDir != "" {
		out.Mode = "configdir"
	}
	if path == "" {
		out.Editable = false
		return out, nil
	}
	if st, err := os.Stat(path); err == nil {
		out.MTime = st.ModTime().UnixNano()
	}
	sec, err := m.readSections()
	if err != nil {
		return out, err
	}
	use := memoryUsage(cur, sec)
	out.BuiltinAgents = sorted(use.builtin)

	backends := sec.backends
	implicit := len(backends) == 0
	if implicit {
		backends = map[string]config.Backend{"main_db": cur.ImplicitMainDB()}
	}
	for _, name := range sortedKeys(backends) {
		v := backendView(name, backends[name])
		v.Implicit = implicit
		v.Profiles = sorted(use.backendProfiles[name])
		v.Agents = sorted(use.backendAgents[name])
		if live, ok := m.boot().Memory.Backends[name]; ok {
			v.Live = live.Provider == backends[name].Provider
		}
		if implicit {
			// The implicit backend's URL is the runtime store's, already
			// expanded: show it masked, never as a value to edit.
			v.URLRef = ""
		}
		out.Backends = append(out.Backends, v)
	}
	for _, name := range sortedKeys(sec.profiles) {
		p := sec.profiles[name]
		v := MemoryProfileView{
			MemoryProfileDraft: profileDraft(name, p),
			Agents:             sorted(use.profileAgents[name]),
			SpawnProfiles:      sorted(use.profileSpawn[name]),
		}
		if live, ok := m.boot().Profiles.Memory[name]; ok {
			v.Live = reflect.DeepEqual(normalizeMemoryProfile(live), normalizeMemoryProfile(p))
		}
		out.Profiles = append(out.Profiles, v)
	}
	return out, nil
}

type memoryUse struct {
	builtin         []string            // agents on the built-in conversational profile
	profileAgents   map[string][]string // profile -> agents
	profileSpawn    map[string][]string // profile -> spawn templates
	backendProfiles map[string][]string // backend -> profiles
	backendAgents   map[string][]string // backend -> agents, through any profile
}

// memoryUsage works out who uses what, so deletes can be refused while
// something still depends on the entry.
func memoryUsage(cfg *config.Config, sec memorySections) memoryUse {
	u := memoryUse{
		profileAgents:   map[string][]string{},
		profileSpawn:    map[string][]string{},
		backendProfiles: map[string][]string{},
		backendAgents:   map[string][]string{},
	}
	for pname, p := range sec.profiles {
		for _, b := range storeBackends(p.Stores) {
			u.backendProfiles[b] = append(u.backendProfiles[b], pname)
		}
	}
	for _, b := range storeBackends(config.DefaultMemoryProfile().Stores) {
		u.backendProfiles[b] = append(u.backendProfiles[b], "conversational")
	}
	for aname, a := range cfg.Agents {
		var stores map[string]config.Store
		switch {
		case a.Memory.IsInline:
			stores = a.Memory.Inline.Stores
		case a.Memory.Profile == "conversational":
			u.builtin = append(u.builtin, aname)
			stores = config.DefaultMemoryProfile().Stores
		case a.Memory.Profile != "":
			u.profileAgents[a.Memory.Profile] = append(u.profileAgents[a.Memory.Profile], aname)
			stores = sec.profiles[a.Memory.Profile].Stores
		}
		for _, b := range storeBackends(stores) {
			u.backendAgents[b] = append(u.backendAgents[b], aname)
		}
	}
	for pname, p := range cfg.Profiles.Agent {
		if p.Memory != "" && p.Memory != "conversational" {
			u.profileSpawn[p.Memory] = append(u.profileSpawn[p.Memory], pname)
		}
	}
	return u
}

func storeBackends(stores map[string]config.Store) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range stores {
		if s.Backend != "" && !seen[s.Backend] {
			seen[s.Backend] = true
			out = append(out, s.Backend)
		}
	}
	sort.Strings(out)
	return out
}

// ValidateProfile dry-runs a profile draft against the deployment.
func (m *Memory) ValidateProfile(d MemoryProfileDraft, creating bool) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	sec, err := m.readSections()
	if err != nil {
		return []string{err.Error()}
	}
	next, errs := m.mergeProfile(sec, d, creating)
	if len(errs) > 0 {
		return errs
	}
	return m.validateSections(next)
}

// ValidateBackend dry-runs a backend draft against the deployment.
func (m *Memory) ValidateBackend(d MemoryBackendDraft, creating bool) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	sec, err := m.readSections()
	if err != nil {
		return []string{err.Error()}
	}
	next, errs := m.mergeBackend(sec, d, creating)
	if len(errs) > 0 {
		return errs
	}
	return m.validateSections(next)
}

// SaveProfile creates or updates one memory profile. expectMTime guards
// against clobbering a hand edit.
func (m *Memory) SaveProfile(d MemoryProfileDraft, creating bool, expectMTime int64) (MemoryList, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commit(expectMTime, func(sec memorySections) (memorySections, []string) {
		return m.mergeProfile(sec, d, creating)
	})
}

// DeleteProfile removes one memory profile, refused while an agent or a spawn
// template names it.
func (m *Memory) DeleteProfile(name string, expectMTime int64) (MemoryList, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commit(expectMTime, func(sec memorySections) (memorySections, []string) {
		if _, ok := sec.profiles[name]; !ok {
			return sec, []string{fmt.Sprintf("memory profile %q does not exist", name)}
		}
		use := memoryUsage(m.current(), sec)
		if users := append(sorted(use.profileAgents[name]), sorted(use.profileSpawn[name])...); len(users) > 0 {
			return sec, []string{fmt.Sprintf("memory profile %q is used by %s; point them at another profile first", name, strings.Join(users, ", "))}
		}
		next := sec.clone()
		delete(next.profiles, name)
		return next, nil
	})
}

// SaveBackend creates or updates one memory backend.
func (m *Memory) SaveBackend(d MemoryBackendDraft, creating bool, expectMTime int64) (MemoryList, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commit(expectMTime, func(sec memorySections) (memorySections, []string) {
		return m.mergeBackend(sec, d, creating)
	})
}

// DeleteBackend removes one memory backend, refused while a profile or an
// agent's stores are on it.
func (m *Memory) DeleteBackend(name string, expectMTime int64) (MemoryList, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commit(expectMTime, func(sec memorySections) (memorySections, []string) {
		if _, ok := sec.backends[name]; !ok {
			if len(sec.backends) == 0 && name == "main_db" {
				return sec, []string{"main_db is the built-in backend on the runtime store; it is not in the file, so there is nothing to delete"}
			}
			return sec, []string{fmt.Sprintf("memory backend %q does not exist", name)}
		}
		use := memoryUsage(m.current(), sec)
		var users []string
		for _, p := range sorted(use.backendProfiles[name]) {
			if p == "conversational" {
				if len(use.builtin) == 0 {
					continue
				}
				p = "the built-in conversational profile (" + strings.Join(sorted(use.builtin), ", ") + ")"
			} else {
				p = "profile " + p
			}
			users = append(users, p)
		}
		for _, a := range sorted(use.backendAgents[name]) {
			if !contains(use.builtin, a) {
				users = append(users, "agent "+a)
			}
		}
		if len(users) > 0 {
			return sec, []string{fmt.Sprintf("memory backend %q is used by %s; move those stores first", name, strings.Join(dedupe(users), ", "))}
		}
		next := sec.clone()
		delete(next.backends, name)
		return next, nil
	})
}

// commit applies one change: conflict check, merge, full validation, write.
func (m *Memory) commit(expectMTime int64, change func(memorySections) (memorySections, []string)) (MemoryList, error) {
	if err := m.checkMTime(expectMTime); err != nil {
		return MemoryList{}, err
	}
	sec, err := m.readSections()
	if err != nil {
		return MemoryList{}, err
	}
	next, errs := change(sec)
	if len(errs) > 0 {
		return MemoryList{}, errors.New(strings.Join(errs, "; "))
	}
	if errs := m.validateSections(next); len(errs) > 0 {
		return MemoryList{}, errors.New(strings.Join(errs, "; "))
	}
	if err := m.writeSections(next); err != nil {
		return MemoryList{}, err
	}
	return m.listLocked()
}

func (m *Memory) mergeProfile(sec memorySections, d MemoryProfileDraft, creating bool) (memorySections, []string) {
	name := strings.TrimSpace(d.Name)
	var errs []string
	switch {
	case name == "":
		errs = append(errs, "a profile name is required")
	case !safetyNameRe.MatchString(name):
		errs = append(errs, "profile name must be 1-64 of letters, digits, _ . -")
	case name == "conversational":
		errs = append(errs, `"conversational" is the built-in profile; choose another name`)
	}
	old, exists := sec.profiles[name]
	if exists && creating {
		errs = append(errs, fmt.Sprintf("memory profile %q already exists", name))
	}
	if !creating && !exists {
		errs = append(errs, fmt.Sprintf("memory profile %q does not exist", name))
	}

	backends := sec.backends
	if len(backends) == 0 {
		backends = map[string]config.Backend{"main_db": {}}
	}
	stores := map[string]config.Store{}
	if len(d.Stores) == 0 {
		errs = append(errs, "a memory profile needs at least one store")
	}
	for i, st := range d.Stores {
		sname := strings.TrimSpace(st.Name)
		label := fmt.Sprintf("store %d", i+1)
		if sname != "" {
			label = fmt.Sprintf("store %q", sname)
		}
		switch {
		case sname == "":
			errs = append(errs, label+": a name is required")
			continue
		case !safetyNameRe.MatchString(sname):
			errs = append(errs, label+": the name must be 1-64 of letters, digits, _ . -")
			continue
		}
		if _, dup := stores[sname]; dup {
			errs = append(errs, label+" appears more than once")
			continue
		}
		backend := strings.TrimSpace(st.Backend)
		if backend == "" {
			errs = append(errs, label+": a backend is required")
		} else if _, ok := backends[backend]; !ok {
			errs = append(errs, fmt.Sprintf("%s: backend %q is not defined", label, backend))
		}
		retention := strings.TrimSpace(st.Retention)
		if retention != "" {
			if _, err := config.ParseRetention(retention); err != nil {
				errs = append(errs, fmt.Sprintf(`%s: retention %q is not a duration (use ms, s, m, h, d, or "forever")`, label, retention))
			}
		}
		if st.Window < 0 {
			errs = append(errs, label+": window cannot be negative")
		}
		scope := strings.TrimSpace(st.Scope)
		if !contains(memoryScopes, scope) {
			errs = append(errs, fmt.Sprintf("%s: scope must be user or agent, not %q", label, scope))
		}
		requires := trimList(st.Requires)
		for _, r := range requires {
			if !contains(memoryRequires, r) {
				errs = append(errs, fmt.Sprintf("%s: %q is not a store feature (kv, text_search, vector)", label, r))
			}
		}
		next := config.Store{
			Backend:    backend,
			Table:      strings.TrimSpace(st.Table),
			Retention:  retention,
			Window:     st.Window,
			Requires:   requires,
			Scope:      scope,
			Pool:       strings.TrimSpace(st.Pool),
			LocalityOK: st.LocalityOK,
		}
		// Keys the editor does not show are kept as they were.
		if prev, ok := old.Stores[sname]; ok {
			next.Collection, next.Policy = prev.Collection, prev.Policy
		}
		stores[sname] = next
	}

	recall := strings.TrimSpace(d.Recall)
	embed := strings.TrimSpace(d.EmbedModel)
	rerank := strings.TrimSpace(d.RerankModel)
	cur := m.current()
	if embed != "" {
		if _, ok := cur.Models[embed]; !ok {
			errs = append(errs, fmt.Sprintf("embed model %q is not a configured model", embed))
		}
	}
	if rerank != "" {
		if mdl, ok := cur.Models[rerank]; !ok {
			errs = append(errs, fmt.Sprintf("rerank model %q is not a configured model", rerank))
		} else if mdl.Provider != "rerank" {
			errs = append(errs, fmt.Sprintf("rerank model %q uses provider %q; a rerank model has provider: rerank", rerank, mdl.Provider))
		}
	}
	if recall == "plugin:semantic" && embed == "" {
		errs = append(errs, "recall plugin:semantic needs an embed model to embed the query")
	}
	if d.Oversample < 0 {
		errs = append(errs, "oversample cannot be negative")
	}
	if len(errs) > 0 {
		return sec, errs
	}
	next := sec.clone()
	next.profiles[name] = config.MemoryProfile{
		Stores:      stores,
		Write:       trimList(d.Write),
		Recall:      recall,
		EmbedModel:  embed,
		RerankModel: rerank,
		Oversample:  d.Oversample,
	}
	return next, nil
}

func (m *Memory) mergeBackend(sec memorySections, d MemoryBackendDraft, creating bool) (memorySections, []string) {
	name := strings.TrimSpace(d.Name)
	provider := strings.TrimSpace(d.Provider)
	var errs []string
	switch {
	case name == "":
		errs = append(errs, "a backend name is required")
	case !safetyNameRe.MatchString(name):
		errs = append(errs, "backend name must be 1-64 of letters, digits, _ . -")
	}
	old, exists := sec.backends[name]
	implicitMain := len(sec.backends) == 0 && name == "main_db"
	if exists && creating {
		errs = append(errs, fmt.Sprintf("memory backend %q already exists", name))
	}
	if !creating && !exists && !implicitMain {
		errs = append(errs, fmt.Sprintf("memory backend %q does not exist", name))
	}
	keys, known := backendKeys[provider]
	if !known {
		errs = append(errs, fmt.Sprintf("provider must be one of %s", strings.Join(config.MemoryProviders(), ", ")))
	}
	if len(errs) > 0 {
		return sec, errs
	}

	cfg := map[string]any{}
	for k, v := range old.Config {
		if !managedBackendKeys[k] || (old.Provider == provider && contains(keys, k)) {
			cfg[k] = v
		}
	}
	setStr := func(key, v string) {
		if !contains(keys, key) {
			return
		}
		if v = strings.TrimSpace(v); v != "" {
			cfg[key] = v
		} else {
			delete(cfg, key)
		}
	}
	setInt := func(key string, v int) {
		if !contains(keys, key) {
			return
		}
		if v > 0 {
			cfg[key] = v
		} else {
			delete(cfg, key)
		}
	}
	setSecret := func(key, v string, clear bool) {
		if !contains(keys, key) {
			return
		}
		switch {
		case strings.TrimSpace(v) != "":
			cfg[key] = strings.TrimSpace(v)
		case clear:
			delete(cfg, key)
		}
	}
	setStr("path", d.Path)
	setStr("collection", d.Collection)
	setStr("distance", d.Distance)
	setStr("timeout", d.Timeout)
	setInt("dim", d.Dim)
	setInt("vector_scan_limit", d.VectorScanLimit)
	setInt("max_entries", d.MaxEntries)
	setSecret("url", d.URL, d.ClearURL)
	setSecret("api_key", d.APIKey, d.ClearAPIKey)

	if contains(keys, "url") {
		if u, _ := cfg["url"].(string); u == "" {
			errs = append(errs, fmt.Sprintf("a %s backend needs a URL", provider))
		}
	}
	if d.Dim < 0 || d.VectorScanLimit < 0 || d.MaxEntries < 0 {
		errs = append(errs, "numbers cannot be negative")
	}
	if len(errs) > 0 {
		return sec, errs
	}
	next := sec.clone()
	if len(next.backends) == 0 && name != "main_db" {
		// The file is about to name its first backend, which ends the
		// implicit main_db. Write it out so agents on the built-in profile
		// keep the store they have.
		next.backends["main_db"] = m.explicitMainDB(sec.persistence)
	}
	if len(cfg) == 0 {
		cfg = nil
	}
	next.backends[name] = config.Backend{Provider: provider, Config: cfg}
	return next, nil
}

// explicitMainDB is the implicit main_db spelled as the file would: the
// runtime store target as written, so a reference stays a reference. Which
// provider it is comes from the loaded config, where references are resolved.
func (m *Memory) explicitMainDB(persistence string) config.Backend {
	implicit := m.current().ImplicitMainDB()
	p := strings.TrimSpace(persistence)
	if p == "" {
		p = "sqlite://./data/agentflow.db"
	}
	if implicit.Provider == "postgres" {
		return config.Backend{Provider: "postgres", Config: map[string]any{"url": p}}
	}
	if config.ParseSecretRef(p).Literal == "" {
		// A reference to a SQLite target: a path is not a secret, so the
		// resolved one is written.
		return implicit
	}
	return config.Backend{Provider: "sqlite", Config: map[string]any{"path": strings.TrimPrefix(p, "sqlite://")}}
}

func (m *Memory) validateSections(next memorySections) []string {
	base, err := m.diskConfig()
	if err != nil {
		return []string{"the current config does not load: " + err.Error()}
	}
	if err := config.ValidateMemorySections(m.sourcePath(), base, next.backends, next.profiles); err != nil {
		return []string{err.Error()}
	}
	return nil
}

func (m *Memory) readSections() (memorySections, error) {
	out := memorySections{backends: map[string]config.Backend{}, profiles: map[string]config.MemoryProfile{}}
	path := m.sourcePath()
	if path == "" {
		return out, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	var frag struct {
		Memory struct {
			Backends map[string]config.Backend `yaml:"backends"`
		} `yaml:"memory"`
		Profiles struct {
			Memory map[string]config.MemoryProfile `yaml:"memory"`
		} `yaml:"profiles"`
		Runtime struct {
			Persistence string `yaml:"persistence"`
		} `yaml:"runtime"`
	}
	if err := yaml.Unmarshal(b, &frag); err != nil {
		return out, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if frag.Memory.Backends != nil {
		out.backends = frag.Memory.Backends
	}
	if frag.Profiles.Memory != nil {
		out.profiles = frag.Profiles.Memory
	}
	out.persistence = frag.Runtime.Persistence
	return out, nil
}

func (m *Memory) checkMTime(expect int64) error {
	path := m.sourcePath()
	if path == "" || expect <= 0 {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrMemoryConflict
		}
		return err
	}
	if st.ModTime().UnixNano() != expect {
		return ErrMemoryConflict
	}
	return nil
}

// writeSections replaces only memory.backends and profiles.memory, leaving
// every other key and comment in place.
func (m *Memory) writeSections(sec memorySections) error {
	path := m.sourcePath()
	if path == "" {
		return errors.New("this instance has no editable config source")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	root, err := config.MappingRoot(&doc)
	if err != nil {
		return err
	}
	if err := applyMemory(root, sec); err != nil {
		return err
	}
	out, err := config.MarshalDocument(&doc)
	if err != nil {
		return err
	}
	if m.configDir == "" {
		return config.ValidateAndSwap(path, out)
	}
	return writeSystemYAML(m.configDir, path, out, m.log)
}

func applyMemory(root *yaml.Node, sec memorySections) error {
	if err := setSubKey(root, "memory", "backends", sec.backends, len(sec.backends) == 0); err != nil {
		return err
	}
	profiles := make(map[string]*yaml.Node, len(sec.profiles))
	for name, p := range sec.profiles {
		n, err := memoryProfileNode(p)
		if err != nil {
			return err
		}
		profiles[name] = n
	}
	return setSubKey(root, "profiles", "memory", profiles, len(profiles) == 0)
}

// setSubKey sets parent.key to v, creating parent when needed, or removes the
// key (and an emptied parent) when empty.
func setSubKey(root *yaml.Node, parent, key string, v any, empty bool) error {
	p := config.MapValue(root, parent)
	if empty {
		if p != nil && p.Kind == yaml.MappingNode {
			config.DeleteMapKey(p, key)
			if len(p.Content) == 0 {
				config.DeleteMapKey(root, parent)
			}
		}
		return nil
	}
	if p == nil || p.Kind != yaml.MappingNode {
		p = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		config.SetMapKey(root, parent, p)
	}
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return err
	}
	config.SetMapKey(p, key, &n)
	return nil
}

// memoryProfileNode encodes a profile without the zero values the struct
// would otherwise spell out, so the file stays hand-readable.
func memoryProfileNode(p config.MemoryProfile) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(p); err != nil {
		return nil, err
	}
	pruneMapping(&n)
	return &n, nil
}

// MemoryProfileMap renders a memory profile as the file spells it, without
// zero values, for clients that write it back as part of an agent.
func MemoryProfileMap(p config.MemoryProfile) map[string]any {
	n, err := memoryProfileNode(p)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := n.Decode(&out); err != nil {
		return nil
	}
	return out
}

func backendView(name string, b config.Backend) MemoryBackendView {
	v := MemoryBackendView{Name: name, Provider: b.Provider, OtherKeys: []string{}, Profiles: []string{}, Agents: []string{}}
	str := func(k string) string { s, _ := b.Config[k].(string); return s }
	num := func(k string) int {
		switch n := b.Config[k].(type) {
		case int:
			return n
		case int64:
			return int(n)
		case uint64:
			return int(n)
		case float64:
			return int(n)
		}
		return 0
	}
	v.Path = str("path")
	v.Collection = str("collection")
	v.Distance = str("distance")
	v.Timeout = str("timeout")
	v.Dim = num("dim")
	v.VectorScanLimit = num("vector_scan_limit")
	v.MaxEntries = num("max_entries")
	if u := str("url"); u != "" {
		v.HasURL = true
		v.URLRef = secretRef(u)
		if v.URLRef == "" {
			v.URLDisplay = maskURL(u)
		}
	}
	if k := str("api_key"); k != "" {
		v.HasAPIKey = true
		v.APIKeyRef = secretRef(k)
	}
	for _, k := range sortedKeys(b.Config) {
		if !managedBackendKeys[k] {
			v.OtherKeys = append(v.OtherKeys, k)
		}
	}
	return v
}

// maskURL keeps a URL's scheme, host and path and hides any password, so the
// editor can say where a backend points without revealing how to log in.
func maskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(set)"
	}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	u.RawQuery = ""
	return u.String()
}

func profileDraft(name string, p config.MemoryProfile) MemoryProfileDraft {
	d := MemoryProfileDraft{
		Name:        name,
		Stores:      []MemoryStoreDraft{},
		Write:       nonNil(p.Write),
		Recall:      p.Recall,
		EmbedModel:  p.EmbedModel,
		RerankModel: p.RerankModel,
		Oversample:  p.Oversample,
	}
	for _, sname := range sortedKeys(p.Stores) {
		s := p.Stores[sname]
		d.Stores = append(d.Stores, MemoryStoreDraft{
			Name: sname, Backend: s.Backend, Table: s.Table, Retention: s.Retention, Window: s.Window,
			Requires: nonNil(s.Requires), Scope: s.Scope, Pool: s.Pool, LocalityOK: s.LocalityOK,
		})
	}
	return d
}

func normalizeMemoryProfile(p config.MemoryProfile) config.MemoryProfile {
	if len(p.Write) == 0 {
		p.Write = nil
	}
	stores := make(map[string]config.Store, len(p.Stores))
	for k, s := range p.Stores {
		if len(s.Requires) == 0 {
			s.Requires = nil
		}
		stores[k] = s
	}
	p.Stores = stores
	return p
}

// modelNames lists the configured models, all of them or one provider's.
func modelNames(cfg *config.Config, provider string) []string {
	out := []string{}
	if cfg == nil {
		return out
	}
	for name, m := range cfg.Models {
		if provider == "" || m.Provider == provider {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
