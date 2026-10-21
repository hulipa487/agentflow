package studio

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"agentflow/internal/config"
	"agentflow/internal/core/safety"
)

// ErrSafetyConflict means the safety source changed since the caller read it.
var ErrSafetyConflict = errors.New("the safety configuration changed on disk since it was read; reload before saving")

// safetyNameRe keeps profile names path- and YAML-safe. Classifier names use
// the config loader's stricter rule (no spaces or ":"), checked separately.
var safetyNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// SafetyProfileDraft is the editable shape of one named filter chain. The
// order of Filters is the order the dispatcher runs them.
type SafetyProfileDraft struct {
	Name    string   `json:"name"`
	Filters []string `json:"filters"`
}

// ClassifierDraft is the editable shape of one model-backed classifier.
// Nil Threshold means the runtime default (0.7); an explicit 0 blocks every
// label, which is why the API keeps the distinction.
type ClassifierDraft struct {
	Name           string            `json:"name"`
	Model          string            `json:"model"`
	Prompt         string            `json:"prompt,omitempty"`
	Mode           string            `json:"mode,omitempty"`
	Directions     []string          `json:"directions,omitempty"`
	Sources        []string          `json:"sources,omitempty"`
	Threshold      *float64          `json:"threshold,omitempty"`
	Categories     map[string]string `json:"categories"`
	Timeout        string            `json:"timeout,omitempty"`
	OnError        string            `json:"on_error,omitempty"`
	OnSaturated    string            `json:"on_saturated,omitempty"`
	OnBudget       string            `json:"on_budget_exhausted,omitempty"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	MaxInputChars  int               `json:"max_input_chars,omitempty"`
	MaxConcurrency int               `json:"max_concurrency,omitempty"`
	Cache          ClassifierCache   `json:"cache"`
	Budget         ClassifierBudget  `json:"budget"`
	Refusal        string            `json:"refusal,omitempty"`
	Fallback       string            `json:"fallback,omitempty"`
}

type ClassifierCache struct {
	Size int    `json:"size,omitempty"`
	TTL  string `json:"ttl,omitempty"`
}

type ClassifierBudget struct {
	TokensPerDay int64 `json:"tokens_per_day,omitempty"`
}

// SafetyProfileView is one profile as the editor sees it, with the usage that
// makes a delete safe or refused.
type SafetyProfileView struct {
	Name          string   `json:"name"`
	Filters       []string `json:"filters"`
	Agents        []string `json:"agents"`
	SpawnProfiles []string `json:"spawn_profiles"`
	Classifiers   []string `json:"classifiers"`
	Live          bool     `json:"live"`
}

// ClassifierView is one model-backed classifier. It carries the same knobs as
// the config, plus the profiles that reference it.
type ClassifierView struct {
	Name           string            `json:"name"`
	Model          string            `json:"model"`
	Prompt         string            `json:"prompt,omitempty"`
	Mode           string            `json:"mode,omitempty"`
	Directions     []string          `json:"directions,omitempty"`
	Sources        []string          `json:"sources,omitempty"`
	Threshold      *float64          `json:"threshold,omitempty"`
	Categories     map[string]string `json:"categories"`
	Timeout        string            `json:"timeout,omitempty"`
	OnError        string            `json:"on_error,omitempty"`
	OnSaturated    string            `json:"on_saturated,omitempty"`
	OnBudget       string            `json:"on_budget_exhausted,omitempty"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	MaxInputChars  int               `json:"max_input_chars,omitempty"`
	MaxConcurrency int               `json:"max_concurrency,omitempty"`
	Cache          ClassifierCache   `json:"cache"`
	Budget         ClassifierBudget  `json:"budget"`
	Refusal        string            `json:"refusal,omitempty"`
	Fallback       string            `json:"fallback,omitempty"`
	UsedBy         []string          `json:"used_by"`
	Live           bool              `json:"live"`
}

// SafetyList is the whole editable safety surface: profiles, classifiers and
// the names an editor may reference.
type SafetyList struct {
	Mode           string              `json:"mode"` // configdir | file
	Path           string              `json:"path,omitempty"`
	Editable       bool                `json:"editable"`
	MTime          int64               `json:"mtime,omitempty"`
	BuiltinFilters []string            `json:"builtin_filters"`
	Profiles       []SafetyProfileView `json:"profiles"`
	Classifiers    []ClassifierView    `json:"classifiers"`
	Models         []string            `json:"models"`
	Prompts        []string            `json:"prompts"`
	Note           string              `json:"note"`
}

// Safety persists safety profiles and model-backed classifiers. Profiles bind
// when an agent's dispatcher is built at boot, so a save is durable but does
// not replace a running agent's chain: the API reports restart_required.
type Safety struct {
	mu         sync.Mutex
	cfg        *config.Config
	configDir  string
	configPath string
	log        *slog.Logger
}

// NewSafety builds the editor over the deployment's config source.
func NewSafety(cfg *config.Config, configDir, configPath string, log *slog.Logger) *Safety {
	if log == nil {
		log = slog.Default()
	}
	if abs, err := filepath.Abs(configDir); err == nil && configDir != "" {
		configDir = abs
	}
	if abs, err := filepath.Abs(configPath); err == nil && configPath != "" {
		configPath = abs
	}
	return &Safety{cfg: cfg, configDir: configDir, configPath: configPath, log: log}
}

// safetySections is the subset of the config this editor owns.
type safetySections struct {
	profiles    map[string]config.SafetyProfile
	classifiers map[string]config.ClassifierProfile
}

func (s safetySections) clone() safetySections {
	out := safetySections{
		profiles:    make(map[string]config.SafetyProfile, len(s.profiles)),
		classifiers: make(map[string]config.ClassifierProfile, len(s.classifiers)),
	}
	for k, v := range s.profiles {
		out.profiles[k] = v
	}
	for k, v := range s.classifiers {
		out.classifiers[k] = v
	}
	return out
}

func (s *Safety) sourcePath() string {
	if s.configDir != "" {
		return filepath.Join(s.configDir, "system.yaml")
	}
	return s.configPath
}

// List reads the persisted safety surface and compares it with the boot
// snapshot, so the caller can see which entries a restart would change.
func (s *Safety) List() (SafetyList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *Safety) listLocked() (SafetyList, error) {
	path := s.sourcePath()
	// The editor offers the models and prompt keys a restart would see. A
	// hand-edited or unsaved-invalid config falls back to the boot snapshot
	// rather than making the whole safety screen unavailable.
	lists := s.boot()
	if disk, err := s.diskConfig(); err == nil && disk != nil {
		lists = disk
	}
	out := SafetyList{
		Mode:           "file",
		Path:           path,
		Editable:       path != "",
		BuiltinFilters: safety.FilterNames(),
		Profiles:       []SafetyProfileView{},
		Classifiers:    []ClassifierView{},
		Models:         classifierModels(lists),
		Prompts:        promptNames(lists),
		Note:           "Safety profiles are core-owned and bind when agentflow starts; saved changes take effect after a restart.",
	}
	if s.configDir != "" {
		out.Mode = "configdir"
	}
	if path == "" {
		out.Editable = false
		return out, nil
	}
	if st, err := os.Stat(path); err == nil {
		out.MTime = st.ModTime().UnixNano()
	}
	sec, err := s.readSections()
	if err != nil {
		return out, err
	}

	agentUse := map[string][]string{}
	for name, a := range s.boot().Agents {
		if a.Safety != "" && a.Safety != "default" && a.Safety != "none" {
			agentUse[a.Safety] = append(agentUse[a.Safety], name)
		}
	}
	spawnUse := map[string][]string{}
	for name, p := range s.boot().Profiles.Agent {
		if p.Safety != "" && p.Safety != "default" && p.Safety != "none" {
			spawnUse[p.Safety] = append(spawnUse[p.Safety], name)
		}
	}
	classifierUse := map[string][]string{}
	for pname, p := range sec.profiles {
		for _, f := range p.Filters {
			if strings.HasPrefix(f, "classifier:") {
				classifierUse[strings.TrimPrefix(f, "classifier:")] = append(
					classifierUse[strings.TrimPrefix(f, "classifier:")], pname)
			}
		}
	}

	for _, name := range sortedKeys(sec.profiles) {
		p := sec.profiles[name]
		v := SafetyProfileView{
			Name:          name,
			Filters:       nonNil(p.Filters),
			Agents:        sorted(agentUse[name]),
			SpawnProfiles: sorted(spawnUse[name]),
			Classifiers:   classifierRefs(p.Filters),
			Live:          profileLive(s.boot(), name, p),
		}
		out.Profiles = append(out.Profiles, v)
	}
	for _, name := range sortedKeys(sec.classifiers) {
		c := sec.classifiers[name]
		v := classifierView(name, c)
		v.UsedBy = sorted(classifierUse[name])
		v.Live = classifierLive(s.boot(), name, c)
		out.Classifiers = append(out.Classifiers, v)
	}
	return out, nil
}

// ValidateProfile dry-runs a profile draft against the deployment.
func (s *Safety) ValidateProfile(d SafetyProfileDraft, creating bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, err := s.readSections()
	if err != nil {
		return []string{err.Error()}
	}
	next, errs := mergeProfile(sec, d, creating)
	if len(errs) > 0 {
		return errs
	}
	return s.validateSections(next)
}

// ValidateClassifier dry-runs a classifier draft against the deployment.
func (s *Safety) ValidateClassifier(d ClassifierDraft, creating bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, err := s.readSections()
	if err != nil {
		return []string{err.Error()}
	}
	next, errs := mergeClassifier(sec, d, creating)
	if len(errs) > 0 {
		return errs
	}
	return s.validateSections(next)
}

// SaveProfile creates or updates one profile. expectMTime guards against
// clobbering a hand edit.
func (s *Safety) SaveProfile(d SafetyProfileDraft, creating bool, expectMTime int64) (SafetyList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkMTime(expectMTime); err != nil {
		return SafetyList{}, err
	}
	sec, err := s.readSections()
	if err != nil {
		return SafetyList{}, err
	}
	next, errs := mergeProfile(sec, d, creating)
	if len(errs) > 0 {
		return SafetyList{}, errors.New(strings.Join(errs, "; "))
	}
	if errs := s.validateSections(next); len(errs) > 0 {
		return SafetyList{}, errors.New(strings.Join(errs, "; "))
	}
	if err := s.writeSections(next); err != nil {
		return SafetyList{}, err
	}
	return s.listLocked()
}

// DeleteProfile removes one profile and refuses when an agent still names it
// (the same reference check a restart runs).
func (s *Safety) DeleteProfile(name string, expectMTime int64) (SafetyList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkMTime(expectMTime); err != nil {
		return SafetyList{}, err
	}
	sec, err := s.readSections()
	if err != nil {
		return SafetyList{}, err
	}
	if _, ok := sec.profiles[name]; !ok {
		return SafetyList{}, fmt.Errorf("safety profile %q does not exist", name)
	}
	next := sec.clone()
	delete(next.profiles, name)
	if errs := s.validateSections(next); len(errs) > 0 {
		return SafetyList{}, errors.New(strings.Join(errs, "; "))
	}
	if err := s.writeSections(next); err != nil {
		return SafetyList{}, err
	}
	return s.listLocked()
}

// SaveClassifier creates or updates one model-backed classifier.
func (s *Safety) SaveClassifier(d ClassifierDraft, creating bool, expectMTime int64) (SafetyList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkMTime(expectMTime); err != nil {
		return SafetyList{}, err
	}
	sec, err := s.readSections()
	if err != nil {
		return SafetyList{}, err
	}
	next, errs := mergeClassifier(sec, d, creating)
	if len(errs) > 0 {
		return SafetyList{}, errors.New(strings.Join(errs, "; "))
	}
	if errs := s.validateSections(next); len(errs) > 0 {
		return SafetyList{}, errors.New(strings.Join(errs, "; "))
	}
	if err := s.writeSections(next); err != nil {
		return SafetyList{}, err
	}
	return s.listLocked()
}

// DeleteClassifier removes one classifier and refuses while a profile still
// references it.
func (s *Safety) DeleteClassifier(name string, expectMTime int64) (SafetyList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkMTime(expectMTime); err != nil {
		return SafetyList{}, err
	}
	sec, err := s.readSections()
	if err != nil {
		return SafetyList{}, err
	}
	if _, ok := sec.classifiers[name]; !ok {
		return SafetyList{}, fmt.Errorf("safety classifier %q does not exist", name)
	}
	next := sec.clone()
	delete(next.classifiers, name)
	if errs := s.validateSections(next); len(errs) > 0 {
		return SafetyList{}, errors.New(strings.Join(errs, "; "))
	}
	if err := s.writeSections(next); err != nil {
		return SafetyList{}, err
	}
	return s.listLocked()
}

func mergeProfile(sec safetySections, d SafetyProfileDraft, creating bool) (safetySections, []string) {
	name := strings.TrimSpace(d.Name)
	var errs []string
	switch {
	case name == "":
		errs = append(errs, "a profile name is required")
	case !safetyNameRe.MatchString(name):
		errs = append(errs, "profile name must be 1-64 of letters, digits, _ . -")
	case name == "default" || name == "none":
		errs = append(errs, fmt.Sprintf("%q is a reserved safety reference; choose another name", name))
	}
	if _, exists := sec.profiles[name]; exists && creating {
		errs = append(errs, fmt.Sprintf("safety profile %q already exists", name))
	}
	if !creating {
		if _, exists := sec.profiles[name]; !exists {
			errs = append(errs, fmt.Sprintf("safety profile %q does not exist", name))
		}
	}
	filters := make([]string, 0, len(d.Filters))
	seen := map[string]bool{}
	for _, f := range d.Filters {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if seen[f] {
			errs = append(errs, fmt.Sprintf("filter %q appears more than once", f))
			continue
		}
		seen[f] = true
		filters = append(filters, f)
	}
	if len(errs) > 0 {
		return safetySections{}, errs
	}
	next := sec.clone()
	next.profiles[name] = config.SafetyProfile{Filters: filters}
	return next, nil
}

func mergeClassifier(sec safetySections, d ClassifierDraft, creating bool) (safetySections, []string) {
	name := strings.TrimSpace(d.Name)
	var errs []string
	switch {
	case name == "":
		errs = append(errs, "a classifier name is required")
	case strings.ContainsAny(name, " \t/:|"):
		errs = append(errs, "classifier name may not contain spaces or : / |")
	case len(name) > 64:
		errs = append(errs, "classifier name must be at most 64 characters")
	}
	if _, exists := sec.classifiers[name]; exists && creating {
		errs = append(errs, fmt.Sprintf("safety classifier %q already exists", name))
	}
	if !creating {
		if _, exists := sec.classifiers[name]; !exists {
			errs = append(errs, fmt.Sprintf("safety classifier %q does not exist", name))
		}
	}
	if len(errs) > 0 {
		return safetySections{}, errs
	}
	next := sec.clone()
	next.classifiers[name] = config.ClassifierProfile{
		Model:          strings.TrimSpace(d.Model),
		Prompt:         strings.TrimSpace(d.Prompt),
		Mode:           strings.TrimSpace(d.Mode),
		Directions:     trimList(d.Directions),
		Sources:        trimList(d.Sources),
		Threshold:      d.Threshold,
		Categories:     normalizeCategories(d.Categories),
		Timeout:        strings.TrimSpace(d.Timeout),
		OnError:        strings.TrimSpace(d.OnError),
		OnSaturated:    strings.TrimSpace(d.OnSaturated),
		OnBudget:       strings.TrimSpace(d.OnBudget),
		MaxTokens:      d.MaxTokens,
		MaxInputChars:  d.MaxInputChars,
		MaxConcurrency: d.MaxConcurrency,
		Cache: config.ClassifierCache{
			Size: d.Cache.Size,
			TTL:  strings.TrimSpace(d.Cache.TTL),
		},
		Budget:   config.ClassifierBudget{TokensPerDay: d.Budget.TokensPerDay},
		Refusal:  strings.TrimSpace(d.Refusal),
		Fallback: strings.TrimSpace(d.Fallback),
	}
	return next, nil
}

func (s *Safety) validateSections(next safetySections) []string {
	base, err := s.diskConfig()
	if err != nil {
		return []string{"the current config does not load: " + err.Error()}
	}
	if err := config.ValidateSafetySections(s.sourcePath(), base, next.profiles, next.classifiers); err != nil {
		return []string{err.Error()}
	}
	return nil
}

// diskConfig loads the source as it is on disk, so validation uses the model
// and prompt set a restart would see rather than the boot snapshot.
func (s *Safety) diskConfig() (*config.Config, error) {
	if s.configDir != "" {
		return config.LoadDir(s.configDir, s.log)
	}
	if s.configPath == "" {
		return s.cfg, nil
	}
	return config.Load(s.configPath)
}

func (s *Safety) boot() *config.Config {
	if s.cfg == nil {
		return &config.Config{}
	}
	return s.cfg
}

func classifierModels(cfg *config.Config) []string {
	out := []string{}
	if cfg == nil {
		return out
	}
	for name, m := range cfg.Models {
		if m.UserKeyed() {
			continue
		}
		switch m.Provider {
		case "anthropic", "openai", "openai-responses", "gemini":
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func promptNames(cfg *config.Config) []string {
	out := []string{}
	if cfg == nil {
		return out
	}
	for name := range cfg.Prompts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (s *Safety) readSections() (safetySections, error) {
	empty := safetySections{profiles: map[string]config.SafetyProfile{}, classifiers: map[string]config.ClassifierProfile{}}
	path := s.sourcePath()
	if path == "" {
		return empty, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	var frag struct {
		Profiles struct {
			Safety map[string]config.SafetyProfile `yaml:"safety"`
		} `yaml:"profiles"`
		Safety config.SafetyConfig `yaml:"safety"`
	}
	if err := yaml.Unmarshal(b, &frag); err != nil {
		return empty, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if frag.Profiles.Safety != nil {
		empty.profiles = frag.Profiles.Safety
	}
	if frag.Safety.Classifiers != nil {
		empty.classifiers = frag.Safety.Classifiers
	}
	for name, p := range empty.profiles {
		if p.Filters == nil {
			p.Filters = []string{}
			empty.profiles[name] = p
		}
	}
	return empty, nil
}

func (s *Safety) checkMTime(expect int64) error {
	path := s.sourcePath()
	if path == "" || expect <= 0 {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrSafetyConflict
		}
		return err
	}
	if st.ModTime().UnixNano() != expect {
		return ErrSafetyConflict
	}
	return nil
}

// writeSections replaces only profiles.safety and safety.classifiers, leaving
// every other key and comment in place.
func (s *Safety) writeSections(sec safetySections) error {
	path := s.sourcePath()
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
	if err := applySafety(root, sec); err != nil {
		return err
	}
	out, err := config.MarshalDocument(&doc)
	if err != nil {
		return err
	}
	if s.configDir == "" {
		return config.ValidateAndSwap(path, out)
	}
	return s.writeDir(path, out)
}

func applySafety(root *yaml.Node, sec safetySections) error {
	if len(sec.classifiers) == 0 {
		config.DeleteMapKey(root, "safety")
	} else {
		var v yaml.Node
		if err := v.Encode(config.SafetyConfig{Classifiers: sec.classifiers}); err != nil {
			return err
		}
		config.SetMapKey(root, "safety", &v)
	}

	profiles := config.MapValue(root, "profiles")
	if len(sec.profiles) == 0 {
		if profiles != nil && profiles.Kind == yaml.MappingNode {
			config.DeleteMapKey(profiles, "safety")
			if len(profiles.Content) == 0 {
				config.DeleteMapKey(root, "profiles")
			}
		}
		return nil
	}
	if profiles == nil || profiles.Kind != yaml.MappingNode {
		profiles = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		config.SetMapKey(root, "profiles", profiles)
	}
	var v yaml.Node
	if err := v.Encode(sec.profiles); err != nil {
		return err
	}
	config.SetMapKey(profiles, "safety", &v)
	return nil
}

// writeDir writes system.yaml and reloads the whole directory, restoring the
// previous file if the new one would not boot.
func (s *Safety) writeDir(path string, out []byte) error {
	old, oldErr := os.ReadFile(path)
	if oldErr != nil && !os.IsNotExist(oldErr) {
		return oldErr
	}
	restore := func() {
		if oldErr == nil {
			_ = os.WriteFile(path, old, 0o600)
		} else {
			_ = os.Remove(path)
		}
	}
	if oldErr == nil {
		if err := os.WriteFile(path+".bak", old, 0o600); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	tmp, err := os.CreateTemp(s.configDir, ".system-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		if err2 := os.WriteFile(path, out, 0o600); err2 != nil {
			return fmt.Errorf("replace system.yaml: %w (fallback: %v)", err, err2)
		}
	}
	_ = os.Chmod(path, 0o600)
	if _, err := config.LoadDir(s.configDir, s.log); err != nil {
		restore()
		return fmt.Errorf("system.yaml does not validate: %w", err)
	}
	return nil
}

func profileLive(cfg *config.Config, name string, p config.SafetyProfile) bool {
	if cfg == nil {
		return false
	}
	live, ok := cfg.Profiles.Safety[name]
	if !ok {
		return false
	}
	return equalStrings(live.Filters, p.Filters)
}

func classifierLive(cfg *config.Config, name string, p config.ClassifierProfile) bool {
	if cfg == nil {
		return false
	}
	live, ok := cfg.Safety.Classifiers[name]
	if !ok {
		return false
	}
	return reflect.DeepEqual(normalizeClassifier(live), normalizeClassifier(p))
}

func normalizeClassifier(p config.ClassifierProfile) config.ClassifierProfile {
	if len(p.Directions) == 0 {
		p.Directions = nil
	}
	if len(p.Sources) == 0 {
		p.Sources = nil
	}
	if len(p.Categories) == 0 {
		p.Categories = nil
	}
	return p
}

func classifierView(name string, c config.ClassifierProfile) ClassifierView {
	return ClassifierView{
		Name:           name,
		Model:          c.Model,
		Prompt:         c.Prompt,
		Mode:           c.Mode,
		Directions:     nonNil(c.Directions),
		Sources:        nonNil(c.Sources),
		Threshold:      c.Threshold,
		Categories:     normalizeCategories(c.Categories),
		Timeout:        c.Timeout,
		OnError:        c.OnError,
		OnSaturated:    c.OnSaturated,
		OnBudget:       c.OnBudget,
		MaxTokens:      c.MaxTokens,
		MaxInputChars:  c.MaxInputChars,
		MaxConcurrency: c.MaxConcurrency,
		Cache:          ClassifierCache{Size: c.Cache.Size, TTL: c.Cache.TTL},
		Budget:         ClassifierBudget{TokensPerDay: c.Budget.TokensPerDay},
		Refusal:        c.Refusal,
		Fallback:       c.Fallback,
		UsedBy:         []string{},
	}
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func trimList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeCategories(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for label, action := range in {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		out[label] = strings.ToLower(strings.TrimSpace(action))
	}
	return out
}

func classifierRefs(filters []string) []string {
	out := []string{}
	for _, f := range filters {
		if strings.HasPrefix(f, "classifier:") {
			out = append(out, strings.TrimPrefix(f, "classifier:"))
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
