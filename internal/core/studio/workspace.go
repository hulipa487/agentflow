package studio

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"agentflow/internal/config"
)

// AgentFiles is where one studio-managed agent lives on disk.
type AgentFiles struct {
	Mode             string `json:"mode"` // configdir | file
	ProfilePath      string `json:"profile_path,omitempty"`
	LoopPath         string `json:"loop_path"`
	InstructionsPath string `json:"instructions_path,omitempty"`
	Managed          bool   `json:"managed"`
	Mtime            int64  `json:"mtime,omitempty"`
}

// ErrConflict means the file changed since the caller read it.
var ErrConflict = errors.New("the agent's file changed on disk since it was read; reload before saving")

// ErrNotManaged refuses to overwrite or delete a profile the Studio did not
// write. A hand-maintained profile is the operator's file.
var ErrNotManaged = errors.New("this profile was not created by Studio; refusing to overwrite or delete it (force to override)")

// managedMarker is the first line of every file the Studio owns.
const managedMarker = "# managed by agentflow studio — edits may be overwritten\n"

var studioNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Workspace persists agents into the deployment's own config. In a -configdir
// deployment that is profiles/*.yaml plus loops/ and prompts/; in a single-file
// deployment it is the agents: map of that file plus a studio/ directory for
// the loop and prompt.
type Workspace struct {
	ConfigDir  string
	ConfigPath string
	Root       string // single-file mode: where loops/prompts live
	Cfg        *config.Config
	Log        *slog.Logger
}

// NewWorkspace builds a workspace. root is runtime.studio.workspace; empty
// means <config dir>/studio for a single-file deployment.
func NewWorkspace(configDir, configPath, root string, cfg *config.Config, log *slog.Logger) *Workspace {
	if log == nil {
		log = slog.Default()
	}
	// Every stored path is absolute: the loader's rebasing and the process CWD
	// are different bases, and a studio write must not depend on where the
	// binary was started.
	if abs, err := filepath.Abs(configDir); err == nil && configDir != "" {
		configDir = abs
	}
	if abs, err := filepath.Abs(configPath); err == nil && configPath != "" {
		configPath = abs
	}
	if root == "" {
		base := configDir
		if base == "" {
			base = filepath.Dir(configPath)
		}
		root = filepath.Join(base, "studio")
	} else if !filepath.IsAbs(root) {
		base := configDir
		if base == "" {
			base = filepath.Dir(configPath)
		}
		root = filepath.Join(base, root)
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Workspace{ConfigDir: configDir, ConfigPath: configPath, Root: root, Cfg: cfg, Log: log.With("module", "studio-workspace")}
}

// Stage writes an agent's loop and instructions to the workspace without
// touching the config. Apply uses it so the runtime definition points at a
// real file (hot reload watches it) even when the agent is not persisted yet.
// The paths returned are absolute and safe to hand to builtins.Resolve.
func (w *Workspace) Stage(name, loopSrc, instructionsSrc string) (loopPath, promptPath string, err error) {
	if !studioNameRe.MatchString(name) {
		return "", "", fmt.Errorf("agent name must be 1-64 chars of letters, digits, _ or -")
	}
	if strings.TrimSpace(loopSrc) == "" {
		return "", "", fmt.Errorf("loop source is empty")
	}
	if w.Mode() == "" {
		return "", "", fmt.Errorf("this instance has no config file or config directory to persist into")
	}
	loopPath = w.loopPath(name)
	promptPath = w.promptPath(name)
	if err := os.MkdirAll(filepath.Dir(loopPath), 0o755); err != nil {
		return "", "", err
	}
	if err := writeAtomic(loopPath, []byte(loopSrc)); err != nil {
		return "", "", err
	}
	if instructionsSrc != "" {
		if err := os.MkdirAll(filepath.Dir(promptPath), 0o755); err != nil {
			return "", "", err
		}
		if err := writeAtomic(promptPath, []byte(instructionsSrc)); err != nil {
			return "", "", err
		}
	} else if err := os.Remove(promptPath); err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	return loopPath, promptPath, nil
}

// Persisted reports whether the agent has a definition on disk.
func (w *Workspace) Persisted(name string) bool {
	switch w.Mode() {
	case "configdir":
		return fileExists(w.profilePath(name))
	case "file":
		root, err := w.configRoot()
		if err != nil {
			return false
		}
		agents := config.MapValue(root, "agents")
		return agents != nil && config.MapValue(agents, name) != nil
	}
	return false
}

// MarshalAgent renders an agent definition in the YAML shape the config's
// agents: entry (and the builder) uses.
func MarshalAgent(a config.Agent) (string, error) {
	n, err := agentNode(a)
	if err != nil {
		return "", err
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{n}}
	b, err := config.MarshalDocument(doc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Mode reports which persistence layout this deployment uses.
func (w *Workspace) Mode() string {
	if w.ConfigDir != "" {
		return "configdir"
	}
	if w.ConfigPath != "" {
		return "file"
	}
	return ""
}

func (w *Workspace) profilePath(name string) string {
	return filepath.Join(w.ConfigDir, "profiles", name+".yaml")
}

func (w *Workspace) loopPath(name string) string {
	if w.ConfigDir != "" {
		return filepath.Join(w.ConfigDir, "loops", name+".lua")
	}
	return filepath.Join(w.Root, "loops", name+".lua")
}

func (w *Workspace) promptPath(name string) string {
	if w.ConfigDir != "" {
		return filepath.Join(w.ConfigDir, "prompts", name+".md")
	}
	return filepath.Join(w.Root, "prompts", name+".md")
}

// Files reports where the agent is (or would be) persisted.
func (w *Workspace) Files(name string) AgentFiles {
	f := AgentFiles{Mode: w.Mode(), LoopPath: w.loopPath(name), InstructionsPath: w.promptPath(name)}
	if w.ConfigDir != "" {
		f.ProfilePath = w.profilePath(name)
	} else {
		f.ProfilePath = w.ConfigPath
	}
	if st, err := os.Stat(f.ProfilePath); err == nil {
		f.Mtime = st.ModTime().Unix()
	}
	f.Managed = w.managed(name)
	return f
}

// Load reads a persisted agent: the YAML definition plus its loop and
// instructions sources, when present.
func (w *Workspace) Load(name string) (a config.Agent, loop, instructions string, f AgentFiles, err error) {
	f = w.Files(name)
	switch w.Mode() {
	case "configdir":
		b, rerr := os.ReadFile(f.ProfilePath)
		if rerr != nil {
			return a, "", "", f, rerr
		}
		var pf profileFile
		if derr := yaml.Unmarshal(b, &pf); derr != nil {
			return a, "", "", f, fmt.Errorf("parse %s: %w", f.ProfilePath, derr)
		}
		a = pf.Agent
	case "file":
		root, rerr := w.configRoot()
		if rerr != nil {
			return a, "", "", f, rerr
		}
		agents := config.MapValue(root, "agents")
		if agents == nil {
			return a, "", "", f, fmt.Errorf("no agents: section in %s", w.ConfigPath)
		}
		n := config.MapValue(agents, name)
		if n == nil {
			return a, "", "", f, fmt.Errorf("agent %q is not in %s", name, w.ConfigPath)
		}
		if derr := n.Decode(&a); derr != nil {
			return a, "", "", f, fmt.Errorf("parse agent %q: %w", name, derr)
		}
	default:
		return a, "", "", f, fmt.Errorf("this instance has no config file or config directory to persist into")
	}
	// A directory fragment stores paths relative to the directory, and a
	// single-file definition resolves relative paths against the config file.
	// Load is the inverse of the loader and must use the same base.
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		if w.ConfigDir != "" {
			return filepath.Join(w.ConfigDir, p)
		}
		if w.ConfigPath != "" {
			return filepath.Join(filepath.Dir(w.ConfigPath), p)
		}
		return p
	}
	if a.Loop != "" {
		if b, rerr := os.ReadFile(resolve(a.Loop)); rerr == nil {
			loop = string(b)
		}
	}
	if a.Instructions.File != "" {
		if b, rerr := os.ReadFile(resolve(a.Instructions.File)); rerr == nil {
			instructions = string(b)
		}
	}
	return a, loop, instructions, f, nil
}

// Save writes the agent's loop, instructions and YAML. The caller has already
// validated the definition against the live runtime; this layer validates that
// the resulting config still loads, and rolls back on failure.
//
// expectMtime is the mtime the caller last saw (0 skips the check); force
// allows replacing a profile the Studio does not own.
func (w *Workspace) Save(name string, a config.Agent, loopSrc, instructionsSrc string, expectMtime int64, force bool) (AgentFiles, error) {
	if !studioNameRe.MatchString(name) {
		return AgentFiles{}, fmt.Errorf("agent name must be 1-64 chars of letters, digits, _ or -")
	}
	f := w.Files(name)
	if w.Mode() == "" {
		return f, fmt.Errorf("this instance has no config file or config directory to persist into")
	}
	if expectMtime != 0 && f.Mtime != 0 && f.Mtime != expectMtime {
		return f, ErrConflict
	}
	if w.Mode() == "configdir" && !force && fileExists(f.ProfilePath) && !w.managed(name) {
		return f, ErrNotManaged
	}

	loopPath := w.loopPath(name)
	promptPath := w.promptPath(name)
	if w.Mode() == "configdir" {
		// Paths in a configdir are rebased onto the directory, so the fragment
		// stores them relative to it.
		a.Loop = filepath.ToSlash(filepath.Join("loops", name+".lua"))
		if instructionsSrc != "" {
			a.Instructions = config.InstructionsRef{File: filepath.ToSlash(filepath.Join("prompts", name+".md"))}
		} else {
			a.Instructions = config.InstructionsRef{}
		}
	} else {
		// Single-file loops and instructions resolve against the config file
		// at load. Persist absolute paths anyway: the definition then survives
		// moving the file or editing it with another tool.
		abs, err := filepath.Abs(loopPath)
		if err != nil {
			return f, err
		}
		a.Loop = abs
		if instructionsSrc != "" {
			absP, err := filepath.Abs(promptPath)
			if err != nil {
				return f, err
			}
			a.Instructions = config.InstructionsRef{File: absP}
		} else {
			a.Instructions = config.InstructionsRef{}
		}
	}

	var undo []undoStep
	rollback := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i].apply()
		}
	}

	// Loop first: the agent definition must not point at a missing file when
	// the config is validated.
	if err := w.writeTracked(loopPath, []byte(loopSrc), &undo); err != nil {
		return f, err
	}
	if instructionsSrc != "" {
		if err := w.writeTracked(promptPath, []byte(instructionsSrc), &undo); err != nil {
			rollback()
			return f, err
		}
	} else if err := w.removeTracked(promptPath, &undo); err != nil {
		return f, err
	}

	switch w.Mode() {
	case "configdir":
		doc, err := buildProfileDoc(name, a)
		if err != nil {
			rollback()
			return f, err
		}
		if err := w.writeTracked(f.ProfilePath, doc, &undo); err != nil {
			rollback()
			return f, err
		}
		if _, err := config.LoadDir(w.ConfigDir, w.Log); err != nil {
			rollback()
			return f, fmt.Errorf("persisted fragment does not load: %w", err)
		}
	case "file":
		doc, err := w.configRoot()
		if err != nil {
			rollback()
			return f, err
		}
		agents := config.MapValue(doc, "agents")
		if agents == nil {
			agents = &yaml.Node{Kind: yaml.MappingNode}
			config.SetMapKey(doc, "agents", agents)
		}
		node, err := agentNode(a)
		if err != nil {
			rollback()
			return f, err
		}
		config.SetMapKey(agents, name, node)
		b, err := w.marshalConfig(doc)
		if err != nil {
			rollback()
			return f, err
		}
		if err := config.ValidateAndSwap(w.ConfigPath, b); err != nil {
			rollback()
			return f, fmt.Errorf("persisted config does not load: %w", err)
		}
	}
	return w.Files(name), nil
}

// Remove deletes a persisted agent and the loop/prompt files it references.
// force overrides the refusal to touch a profile the Studio does not own.
func (w *Workspace) Remove(name string, force bool) ([]string, error) {
	if !studioNameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid agent name")
	}
	f := w.Files(name)
	var removed []string
	switch w.Mode() {
	case "configdir":
		if !fileExists(f.ProfilePath) {
			return nil, os.ErrNotExist
		}
		if !force && !w.managed(name) {
			return nil, ErrNotManaged
		}
		if err := os.Remove(f.ProfilePath); err != nil {
			return nil, err
		}
		removed = append(removed, f.ProfilePath)
		for _, p := range []string{w.loopPath(name), w.promptPath(name)} {
			if fileExists(p) {
				if err := os.Remove(p); err == nil {
					removed = append(removed, p)
				}
			}
		}
	case "file":
		doc, err := w.configRoot()
		if err != nil {
			return nil, err
		}
		agents := config.MapValue(doc, "agents")
		if agents == nil || !config.DeleteMapKey(agents, name) {
			return nil, os.ErrNotExist
		}
		b, err := w.marshalConfig(doc)
		if err != nil {
			return nil, err
		}
		if err := config.ValidateAndSwap(w.ConfigPath, b); err != nil {
			return nil, err
		}
		removed = append(removed, w.ConfigPath)
		for _, p := range []string{w.loopPath(name), w.promptPath(name)} {
			if fileExists(p) {
				if err := os.Remove(p); err == nil {
					removed = append(removed, p)
				}
			}
		}
	default:
		return nil, fmt.Errorf("this instance has no config file or config directory to persist into")
	}
	return removed, nil
}

func (w *Workspace) managed(name string) bool {
	if w.Mode() != "configdir" {
		// A single-file deployment cannot mark one key of the document.
		return true
	}
	b, err := os.ReadFile(w.profilePath(name))
	return err == nil && strings.HasPrefix(string(b), managedMarker)
}

func (w *Workspace) configRoot() (*yaml.Node, error) {
	b, err := os.ReadFile(w.ConfigPath)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", w.ConfigPath, err)
	}
	return config.MappingRoot(&doc)
}

func (w *Workspace) marshalConfig(root *yaml.Node) ([]byte, error) {
	// Re-wrap the mapping in a document so the encoder emits a normal file.
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	return config.MarshalDocument(doc)
}

// profileFile is one profiles/*.yaml fragment.
type profileFile struct {
	Name         string `yaml:"name"`
	Spawn        bool   `yaml:"spawn"`
	config.Agent `yaml:",inline"`
}

func buildProfileDoc(name string, a config.Agent) ([]byte, error) {
	n, err := agentNode(a)
	if err != nil {
		return nil, err
	}
	root := &yaml.Node{Kind: yaml.MappingNode}
	root.Content = append(root.Content,
		scalar("name"), scalar(name),
		scalar("spawn"), scalar("false"))
	root.Content = append(root.Content, n.Content...)
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	b, err := config.MarshalDocument(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte(managedMarker), b...), nil
}

// agentNode renders an Agent as a mapping node, fixing the instructions field:
// InstructionsRef marshals as {file, prompt}, which its own decoder rejects.
// The persisted form is the scalar path or the {prompt: key} mapping the
// config schema accepts.
func agentNode(a config.Agent) (*yaml.Node, error) {
	b, err := yaml.Marshal(a)
	if err != nil {
		return nil, err
	}
	var n yaml.Node
	if err := yaml.Unmarshal(b, &n); err != nil {
		return nil, err
	}
	if len(n.Content) == 0 || n.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("agent did not encode as a mapping")
	}
	root := n.Content[0]
	// The Agent struct has no omitempty tags (the config decoder is strict but
	// not sparse), so a raw marshal writes every zero value. A persisted
	// profile is a hand-readable file first; drop the defaults, keeping the one
	// boolean whose zero value is meaningful (inherit_user: false opts out).
	pruneMapping(root)
	switch {
	case a.Instructions.File != "":
		config.SetMapKey(root, "instructions", scalar(a.Instructions.File))
	case a.Instructions.Prompt != "":
		m := &yaml.Node{Kind: yaml.MappingNode}
		config.SetMapKey(m, "prompt", scalar(a.Instructions.Prompt))
		config.SetMapKey(root, "instructions", m)
	default:
		config.DeleteMapKey(root, "instructions")
	}
	return root, nil
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }

// pruneMapping removes entries that only restate a default.
func pruneMapping(root *yaml.Node) {
	out := root.Content[:0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if !pruneValue(k.Value, v) {
			continue
		}
		out = append(out, k, v)
	}
	root.Content = out
}

func pruneValue(key string, v *yaml.Node) bool {
	switch v.Kind {
	case yaml.ScalarNode:
		if v.Value == "" || v.Value == "null" {
			return false
		}
		// inherit_user: false is an explicit opt-out, not a default.
		if v.Value == "false" && key != "inherit_user" {
			return false
		}
		if (key == "history_budget" || key == "oversample") && v.Value == "0" {
			return false
		}
		return true
	case yaml.SequenceNode:
		return len(v.Content) > 0
	case yaml.MappingNode:
		pruneMapping(v)
		return len(v.Content) > 0
	}
	return true
}

type undoStep struct{ apply func() }

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// writeTracked writes a file and records how to restore its previous contents
// (or remove it) if a later step fails.
func (w *Workspace) writeTracked(path string, data []byte, undo *[]undoStep) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	existed := err == nil
	if err := writeAtomic(path, data); err != nil {
		return err
	}
	*undo = append(*undo, undoStep{apply: func() {
		if existed {
			_ = os.WriteFile(path, old, 0o600)
		} else {
			_ = os.Remove(path)
		}
	}})
	return nil
}

func (w *Workspace) removeTracked(path string, undo *[]undoStep) error {
	old, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	*undo = append(*undo, undoStep{apply: func() { _ = os.WriteFile(path, old, 0o600) }})
	return nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".studio-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", old, 0o600)
	}
	if err := os.Rename(name, path); err != nil {
		if err2 := os.WriteFile(path, data, 0o600); err2 != nil {
			return fmt.Errorf("replace %s: %w (fallback: %v)", path, err, err2)
		}
	}
	return nil
}
