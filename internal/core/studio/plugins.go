package studio

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"agentflow/internal/config"
	"agentflow/internal/luacode"
	"agentflow/internal/vm"
)

// ErrPluginConflict means a plugin or package file changed on disk since the
// caller read it.
var ErrPluginConflict = errors.New("the Lua file changed on disk since it was read; reload before saving")

var (
	pluginPackageNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	pluginFileNameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}\.lua$`)
)

// builtinSummaries describes each runtime builtin surfaced in Studio.
var builtinSummaries = map[string]string{
	"token_budget":  "Default token_budget(messages, budget) helper",
	"routing_table": "Default memory.write filter routing turn and fact records",
	"recency":       "Default memory.recall handler for recent, prefix, text and time queries",
	"semantic":      "Default memory.recall handler using embeddings and vector search",
	"per_chat":      "Default gateway.route handler isolating sessions per user/chat",
}

// BuiltinPluginView is one built-in runtime Lua chunk and any plugins.dir
// shadow overriding it on disk.
type BuiltinPluginView struct {
	Name          string `json:"name"`
	Kind          string `json:"kind"` // support | route
	Summary       string `json:"summary"`
	Shadowed      bool   `json:"shadowed"`
	Path          string `json:"path,omitempty"`
	MTime         int64  `json:"mtime,omitempty"`
	Source        string `json:"source"`
	BuiltinSource string `json:"builtin_source"`
}

// PackageFileView is one *.lua file inside a configured package directory.
type PackageFileView struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	MTime  int64  `json:"mtime,omitempty"`
	Source string `json:"source"`
}

// PackageView is one declared shared Lua package (config `packages:`).
type PackageView struct {
	Name    string            `json:"name"`
	Dir     string            `json:"dir"`
	Managed bool              `json:"managed"`
	Files   []PackageFileView `json:"files"`
}

// PluginsList is the full Shared & Built-in Lua surface for Studio.
type PluginsList struct {
	Mode                string              `json:"mode"` // configdir | file
	Editable            bool                `json:"editable"`
	ConfigPath          string              `json:"config_path,omitempty"`
	PluginDir           string              `json:"plugin_dir,omitempty"`
	PluginDirConfigured bool                `json:"plugin_dir_configured"`
	PreludeVersion      int                 `json:"prelude_version"`
	Builtins            []BuiltinPluginView `json:"builtins"`
	Packages            []PackageView       `json:"packages"`
	Note                string              `json:"note,omitempty"`
}

// PluginHooks apply live runtime updates after Studio writes a plugin or
// package file. All callbacks are optional.
type PluginHooks struct {
	OnReloadSupport  func()
	OnReloadRoute    func()
	OnReloadPackages func()
}

// Plugins manages plugins.dir overrides and shared `packages:` Lua files on
// disk, keeping the config YAML, luacode registry, live runtime and reload
// watcher in sync.
type Plugins struct {
	mu         sync.Mutex
	cfg        *config.Config
	configDir  string
	configPath string
	hooks      PluginHooks
	log        *slog.Logger
}

// NewPlugins builds a Plugins editor over the deployment's config source.
func NewPlugins(cfg *config.Config, configDir, configPath string, hooks PluginHooks, log *slog.Logger) *Plugins {
	if log == nil {
		log = slog.Default()
	}
	if abs, err := filepath.Abs(configDir); err == nil && configDir != "" {
		configDir = abs
	}
	if abs, err := filepath.Abs(configPath); err == nil && configPath != "" {
		configPath = abs
	}
	return &Plugins{
		cfg:        cfg,
		configDir:  configDir,
		configPath: configPath,
		hooks:      hooks,
		log:        log.With("module", "studio-plugins"),
	}
}

func (p *Plugins) mode() string {
	switch {
	case p.configDir != "":
		return "configdir"
	case p.configPath != "":
		return "file"
	default:
		return ""
	}
}

func (p *Plugins) sourcePath() string {
	if p.configDir != "" {
		return filepath.Join(p.configDir, "system.yaml")
	}
	return p.configPath
}

func (p *Plugins) baseDir() string {
	if p.configDir != "" {
		return p.configDir
	}
	if p.configPath != "" {
		return filepath.Dir(p.configPath)
	}
	return ""
}

type pluginSections struct {
	pluginDir           string // rebased absolute path when configured
	pluginDirConfigured bool
	rawPluginDir        string
	packages            map[string]string // name -> rebased absolute path
	rawPackages         map[string]string // name -> raw YAML path
}

func (p *Plugins) readSections() (pluginSections, error) {
	sec := pluginSections{
		packages:    map[string]string{},
		rawPackages: map[string]string{},
	}
	base := p.baseDir()
	path := p.sourcePath()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return sec, err
		}
		if err == nil {
			var frag struct {
				Plugins struct {
					Dir string `yaml:"dir"`
				} `yaml:"plugins"`
				Packages map[string]string `yaml:"packages"`
			}
			if err := yaml.Unmarshal(b, &frag); err != nil {
				return sec, fmt.Errorf("%s: %w", filepath.Base(path), err)
			}
			sec.rawPluginDir = strings.TrimSpace(frag.Plugins.Dir)
			if sec.rawPluginDir != "" {
				sec.pluginDirConfigured = true
				sec.pluginDir = resolvePluginPath(base, sec.rawPluginDir)
			}
			for name, rawDir := range frag.Packages {
				rawDir = strings.TrimSpace(rawDir)
				if rawDir == "" {
					continue
				}
				sec.rawPackages[name] = rawDir
				sec.packages[name] = resolvePluginPath(base, rawDir)
			}
			return sec, nil
		}
	}
	if p.cfg != nil {
		if d := strings.TrimSpace(p.cfg.Plugins.Dir); d != "" {
			sec.pluginDirConfigured = true
			sec.rawPluginDir = d
			sec.pluginDir = resolvePluginPath(base, d)
		}
		for name, dir := range p.cfg.Packages {
			if dir != "" {
				sec.rawPackages[name] = dir
				sec.packages[name] = resolvePluginPath(base, dir)
			}
		}
	}
	return sec, nil
}

func resolvePluginPath(base, raw string) string {
	if raw == "" {
		return ""
	}
	cleaned := filepath.FromSlash(raw)
	if filepath.IsAbs(cleaned) || base == "" {
		if abs, err := filepath.Abs(cleaned); err == nil {
			return abs
		}
		return cleaned
	}
	return filepath.Clean(filepath.Join(base, cleaned))
}

// List returns all runtime builtins (with any disk overrides) and all declared
// shared packages and their *.lua files.
func (p *Plugins) List() (PluginsList, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listLocked()
}

func (p *Plugins) listLocked() (PluginsList, error) {
	sec, err := p.readSections()
	if err != nil {
		return PluginsList{}, err
	}
	base := p.baseDir()
	displayDir := sec.pluginDir
	if displayDir == "" && base != "" {
		displayDir = filepath.Join(base, "plugins")
	}

	supportSet := map[string]bool{}
	for _, n := range luacode.SupportOrder() {
		supportSet[n] = true
	}

	names := luacode.BuiltinNames()
	builtins := make([]BuiltinPluginView, 0, len(names))
	for _, name := range names {
		builtinSrc, _ := luacode.BuiltinSource(name)
		kind := "route"
		if supportSet[name] {
			kind = "support"
		}
		view := BuiltinPluginView{
			Name:          name,
			Kind:          kind,
			Summary:       builtinSummaries[name],
			Source:        builtinSrc,
			BuiltinSource: builtinSrc,
		}
		if sec.pluginDirConfigured && sec.pluginDir != "" {
			candidate := filepath.Join(sec.pluginDir, name+".lua")
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				b, err := os.ReadFile(candidate)
				if err != nil {
					return PluginsList{}, fmt.Errorf("read %s: %w", candidate, err)
				}
				view.Shadowed = true
				view.Path = candidate
				view.MTime = st.ModTime().UnixNano()
				view.Source = string(b)
			}
		}
		builtins = append(builtins, view)
	}

	managedPkgsRoot := ""
	if base != "" {
		managedPkgsRoot = filepath.Join(base, "packages")
	}
	pkgNames := sortedKeys(sec.packages)
	pkgs := make([]PackageView, 0, len(pkgNames))
	for _, name := range pkgNames {
		dir := sec.packages[name]
		files, err := readPackageFiles(dir)
		if err != nil && !os.IsNotExist(err) {
			return PluginsList{}, fmt.Errorf("package %q: %w", name, err)
		}
		if files == nil {
			files = []PackageFileView{}
		}
		managed := managedPkgsRoot != "" && isSubpath(managedPkgsRoot, dir)
		pkgs = append(pkgs, PackageView{
			Name:    name,
			Dir:     dir,
			Managed: managed,
			Files:   files,
		})
	}

	out := PluginsList{
		Mode:                p.mode(),
		Editable:            p.sourcePath() != "",
		ConfigPath:          p.sourcePath(),
		PluginDir:           displayDir,
		PluginDirConfigured: sec.pluginDirConfigured,
		PreludeVersion:      vm.PreludeVersion,
		Builtins:            builtins,
		Packages:            pkgs,
	}
	if !out.Editable {
		out.Note = "this instance has no editable config source; start AgentFlow with -config to edit shared plugins"
	}
	return out, nil
}

func readPackageFiles(dir string) ([]PackageFileView, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".lua") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	files := make([]PackageFileView, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		files = append(files, PackageFileView{
			Name:   name,
			Path:   path,
			MTime:  st.ModTime().UnixNano(),
			Source: string(b),
		})
	}
	return files, nil
}

// Validate checks metadata and Lua compilation for a builtin override or a
// shared package file without touching disk.
func (p *Plugins) Validate(kind, name, file, source string) []string {
	var errs []string
	switch kind {
	case "builtin":
		if _, ok := luacode.BuiltinSource(name); !ok {
			errs = append(errs, fmt.Sprintf("unknown built-in plugin %q", name))
			return errs
		}
		if err := luacode.CheckSource("plugin:"+name, source, ""); err != nil {
			errs = append(errs, err.Error())
		}
	case "package":
		if !pluginPackageNameRe.MatchString(name) {
			errs = append(errs, "package name must be 1-64 characters of letters, digits, ., _ or -")
		}
		if file == "" {
			file = "init.lua"
		}
		if err := checkLuaFilename(file); err != nil {
			errs = append(errs, err.Error())
		}
		if err := luacode.CheckSource("package:"+name+"/"+file, source, name); err != nil {
			errs = append(errs, err.Error())
		}
	default:
		errs = append(errs, "kind must be builtin or package")
	}
	return errs
}

// SaveBuiltin writes an override file for a built-in plugin into plugins.dir
// (enabling `plugins.dir: ./plugins` automatically if unset) and triggers live
// reload of either sessions or the router.
func (p *Plugins) SaveBuiltin(name, source string, expectMTime int64) (PluginsList, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sourcePath() == "" {
		return PluginsList{}, errors.New("this instance has no editable config source")
	}
	if _, ok := luacode.BuiltinSource(name); !ok {
		return PluginsList{}, fmt.Errorf("unknown built-in plugin %q", name)
	}
	if err := luacode.CheckSource(name+".lua", source, ""); err != nil {
		return PluginsList{}, err
	}

	sec, err := p.readSections()
	if err != nil {
		return PluginsList{}, err
	}
	pluginDir := sec.pluginDir
	if !sec.pluginDirConfigured || pluginDir == "" {
		base := p.baseDir()
		pluginDir = filepath.Join(base, "plugins")
		if err := os.MkdirAll(pluginDir, 0o755); err != nil {
			return PluginsList{}, fmt.Errorf("create plugins dir: %w", err)
		}
		if err := p.setPluginDirInConfig("./plugins"); err != nil {
			return PluginsList{}, err
		}
		luacode.SetPluginDir(pluginDir)
	} else {
		if err := os.MkdirAll(pluginDir, 0o755); err != nil {
			return PluginsList{}, fmt.Errorf("create plugins dir: %w", err)
		}
		luacode.SetPluginDir(pluginDir)
	}

	target := filepath.Join(pluginDir, name+".lua")
	if err := checkFileMTime(target, expectMTime); err != nil {
		return PluginsList{}, err
	}
	if err := writeAtomic(target, []byte(normalizeLF(source))); err != nil {
		return PluginsList{}, err
	}
	p.triggerBuiltinReload(name)
	return p.listLocked()
}

// ResetBuiltin deletes a plugins.dir override file so the runtime reverts to
// the embedded built-in source and triggers a live reload.
func (p *Plugins) ResetBuiltin(name string, expectMTime int64) (PluginsList, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := luacode.BuiltinSource(name); !ok {
		return PluginsList{}, fmt.Errorf("unknown built-in plugin %q", name)
	}
	sec, err := p.readSections()
	if err != nil {
		return PluginsList{}, err
	}
	if !sec.pluginDirConfigured || sec.pluginDir == "" {
		return p.listLocked()
	}
	target := filepath.Join(sec.pluginDir, name+".lua")
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return p.listLocked()
	}
	if err := checkFileMTime(target, expectMTime); err != nil {
		return PluginsList{}, err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return PluginsList{}, err
	}
	_ = os.Remove(target + ".bak")
	p.triggerBuiltinReload(name)
	return p.listLocked()
}

// CreatePackage creates a managed package directory under `<base>/packages/<name>`
// with an initial `.lua` file, registers it under `packages:` in config, and
// refreshes live packages.
func (p *Plugins) CreatePackage(name, file, source string) (PluginsList, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sourcePath() == "" {
		return PluginsList{}, errors.New("this instance has no editable config source")
	}
	if !pluginPackageNameRe.MatchString(name) {
		return PluginsList{}, errors.New("package name must be 1-64 characters of letters, digits, ., _ or -")
	}
	if file == "" {
		file = "init.lua"
	}
	if err := checkLuaFilename(file); err != nil {
		return PluginsList{}, err
	}
	if strings.TrimSpace(source) == "" {
		source = fmt.Sprintf("-- af-prelude-version: %d\n-- Shared Lua package: %s\n\nlocal M = {}\n\nreturn M\n", vm.PreludeVersion, name)
	}
	if err := luacode.CheckSource("package:"+name+"/"+file, source, name); err != nil {
		return PluginsList{}, err
	}

	sec, err := p.readSections()
	if err != nil {
		return PluginsList{}, err
	}
	if _, exists := sec.packages[name]; exists {
		return PluginsList{}, fmt.Errorf("package %q already exists", name)
	}

	base := p.baseDir()
	pkgDir := filepath.Join(base, "packages", name)
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return PluginsList{}, fmt.Errorf("create package dir: %w", err)
	}
	target := filepath.Join(pkgDir, file)
	if err := writeAtomic(target, []byte(normalizeLF(source))); err != nil {
		return PluginsList{}, err
	}

	rawNext := copyStringMap(sec.rawPackages)
	rawNext[name] = "./packages/" + name
	if err := p.setPackagesInConfig(rawNext); err != nil {
		return PluginsList{}, err
	}
	absNext := copyStringMap(sec.packages)
	absNext[name] = pkgDir
	luacode.SetPackages(absNext)

	if p.hooks.OnReloadPackages != nil {
		p.hooks.OnReloadPackages()
	}
	return p.listLocked()
}

// SavePackageFile creates or updates one `.lua` file inside an existing
// package directory, checks syntax/prelude version, and reloads sessions.
func (p *Plugins) SavePackageFile(pkgName, fileName, source string, expectMTime int64) (PluginsList, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sourcePath() == "" {
		return PluginsList{}, errors.New("this instance has no editable config source")
	}
	if err := checkLuaFilename(fileName); err != nil {
		return PluginsList{}, err
	}
	sec, err := p.readSections()
	if err != nil {
		return PluginsList{}, err
	}
	pkgDir, ok := sec.packages[pkgName]
	if !ok {
		return PluginsList{}, fmt.Errorf("unknown package %q", pkgName)
	}
	if err := luacode.CheckSource("package:"+pkgName+"/"+fileName, source, pkgName); err != nil {
		return PluginsList{}, err
	}
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return PluginsList{}, fmt.Errorf("create package dir: %w", err)
	}
	target := filepath.Join(pkgDir, fileName)
	if err := checkFileMTime(target, expectMTime); err != nil {
		return PluginsList{}, err
	}
	if err := writeAtomic(target, []byte(normalizeLF(source))); err != nil {
		return PluginsList{}, err
	}
	luacode.SetPackages(sec.packages)
	if p.hooks.OnReloadPackages != nil {
		p.hooks.OnReloadPackages()
	}
	return p.listLocked()
}

// DeletePackageFile removes one `.lua` file from a package directory while
// keeping at least one `.lua` file intact so configured loops remain resolvable.
func (p *Plugins) DeletePackageFile(pkgName, fileName string, expectMTime int64) (PluginsList, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := checkLuaFilename(fileName); err != nil {
		return PluginsList{}, err
	}
	sec, err := p.readSections()
	if err != nil {
		return PluginsList{}, err
	}
	pkgDir, ok := sec.packages[pkgName]
	if !ok {
		return PluginsList{}, fmt.Errorf("unknown package %q", pkgName)
	}
	files, err := readPackageFiles(pkgDir)
	if err != nil {
		return PluginsList{}, err
	}
	target := filepath.Join(pkgDir, fileName)
	found := false
	for _, f := range files {
		if f.Name == fileName {
			found = true
			break
		}
	}
	if !found {
		return PluginsList{}, os.ErrNotExist
	}
	if len(files) <= 1 {
		return PluginsList{}, errors.New("cannot delete the only .lua file in a package; delete the package instead")
	}
	if err := checkFileMTime(target, expectMTime); err != nil {
		return PluginsList{}, err
	}
	if err := os.Remove(target); err != nil {
		return PluginsList{}, err
	}
	_ = os.Remove(target + ".bak")
	if p.hooks.OnReloadPackages != nil {
		p.hooks.OnReloadPackages()
	}
	return p.listLocked()
}

// DeletePackage unregisters a package from config (refusing if any configured
// loop, spawn profile, route, plugin shadow or sibling package still imports
// it), removes its managed directory under `<base>/packages/`, and reloads.
func (p *Plugins) DeletePackage(pkgName string) (PluginsList, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sourcePath() == "" {
		return PluginsList{}, errors.New("this instance has no editable config source")
	}
	sec, err := p.readSections()
	if err != nil {
		return PluginsList{}, err
	}
	pkgDir, ok := sec.packages[pkgName]
	if !ok {
		return PluginsList{}, fmt.Errorf("unknown package %q", pkgName)
	}
	if users, err := p.findPackageImporters(pkgName, sec); err != nil {
		return PluginsList{}, err
	} else if len(users) > 0 {
		return PluginsList{}, fmt.Errorf("package %q is still imported by: %s", pkgName, strings.Join(users, ", "))
	}

	rawNext := copyStringMap(sec.rawPackages)
	delete(rawNext, pkgName)
	if err := p.setPackagesInConfig(rawNext); err != nil {
		return PluginsList{}, err
	}
	absNext := copyStringMap(sec.packages)
	delete(absNext, pkgName)
	luacode.SetPackages(absNext)

	if base := p.baseDir(); base != "" && isSubpath(filepath.Join(base, "packages"), pkgDir) {
		_ = os.RemoveAll(pkgDir)
	}
	if p.hooks.OnReloadPackages != nil {
		p.hooks.OnReloadPackages()
	}
	return p.listLocked()
}

func (p *Plugins) findPackageImporters(pkgName string, sec pluginSections) ([]string, error) {
	var cfg *config.Config
	var err error
	if p.configDir != "" {
		cfg, err = config.LoadDir(p.configDir, p.log)
	} else if p.configPath != "" {
		cfg, err = config.Load(p.configPath)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect config before removing package: %w", err)
	}
	if cfg == nil {
		return nil, nil
	}

	var users []string
	checkRef := func(label, ref string) {
		if ref == "" {
			return
		}
		if ref == "package:"+pkgName {
			users = append(users, label)
			return
		}
		if strings.HasPrefix(ref, "plugin:") {
			return
		}
		if refImportsPackage(ref, pkgName) {
			users = append(users, label)
		}
	}
	for _, name := range sortedKeys(cfg.Agents) {
		checkRef("agent "+name, cfg.Agents[name].Loop)
	}
	for _, name := range sortedKeys(cfg.Profiles.Agent) {
		checkRef("profile "+name, cfg.Profiles.Agent[name].Loop)
	}
	if cfg.Gateway.Route != "" {
		checkRef("gateway.route", cfg.Gateway.Route)
	}
	if sec.pluginDirConfigured && sec.pluginDir != "" {
		if entries, err := os.ReadDir(sec.pluginDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".lua") {
					if refImportsPackage(filepath.Join(sec.pluginDir, e.Name()), pkgName) {
						users = append(users, "plugin:"+strings.TrimSuffix(e.Name(), ".lua"))
					}
				}
			}
		}
	}
	for _, other := range sortedKeys(sec.packages) {
		if other == pkgName {
			continue
		}
		if refImportsPackage(sec.packages[other], pkgName) {
			users = append(users, "package "+other)
		}
	}
	return dedupe(users), nil
}

func refImportsPackage(path, pkgName string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	checkFile := func(p string) bool {
		b, err := os.ReadFile(p)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if rest, ok := strings.CutPrefix(trimmed, "-- af-import:"); ok {
				if strings.TrimSpace(rest) == pkgName {
					return true
				}
			}
		}
		return false
	}
	if !fi.IsDir() {
		return checkFile(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".lua") {
			if checkFile(filepath.Join(path, e.Name())) {
				return true
			}
		}
	}
	return false
}

func (p *Plugins) triggerBuiltinReload(name string) {
	isSupport := false
	for _, s := range luacode.SupportOrder() {
		if s == name {
			isSupport = true
			break
		}
	}
	if isSupport {
		if p.hooks.OnReloadSupport != nil {
			p.hooks.OnReloadSupport()
		}
		return
	}
	if p.hooks.OnReloadRoute != nil {
		p.hooks.OnReloadRoute()
	}
}

func (p *Plugins) setPluginDirInConfig(dir string) error {
	return p.editConfigYAML(func(root *yaml.Node) error {
		return setSubKey(root, "plugins", "dir", dir, dir == "")
	})
}

func (p *Plugins) setPackagesInConfig(pkgs map[string]string) error {
	return p.editConfigYAML(func(root *yaml.Node) error {
		if len(pkgs) == 0 {
			config.DeleteMapKey(root, "packages")
			return nil
		}
		var v yaml.Node
		if err := v.Encode(pkgs); err != nil {
			return err
		}
		config.SetMapKey(root, "packages", &v)
		return nil
	})
}

func (p *Plugins) editConfigYAML(mutate func(root *yaml.Node) error) error {
	path := p.sourcePath()
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
	if err := mutate(root); err != nil {
		return err
	}
	out, err := config.MarshalDocument(&doc)
	if err != nil {
		return err
	}
	if p.configDir == "" {
		return config.ValidateAndSwap(path, out)
	}
	return writeSystemYAML(p.configDir, path, out, p.log)
}

func checkLuaFilename(name string) error {
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("file name must be a plain filename without path separators")
	}
	if !pluginFileNameRe.MatchString(name) {
		return fmt.Errorf("file name must be 1-64 characters of letters, digits, ., _ or - and end in .lua")
	}
	return nil
}

func checkFileMTime(path string, expectMTime int64) error {
	if expectMTime <= 0 {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrPluginConflict
		}
		return err
	}
	if st.ModTime().UnixNano() != expectMTime {
		return ErrPluginConflict
	}
	return nil
}

func isSubpath(parent, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func normalizeLF(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}
