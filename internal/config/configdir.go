// Config directories: an alternative to the single agentflow.yaml file.
//
// A deployment may instead pass -configdir <dir> and lay its configuration
// out as fragments:
//
//	system.yaml     -> the top-level Config minus agents/channels (required)
//	channels.yaml   -> `channels: [...]` (optional)
//	profiles/*.yaml -> one agent per file (optional; sorted-glob load)
//	triggers/*.yaml -> `triggers: [...]` (optional; merged sorted, later wins)
//
// Every fragment decodes under the same strict KnownFields validation as the
// single-file path, and validation runs only after the merge, so cross-file
// references (can_contact, workflow step targets, channel agents) check
// exactly as they do today. <dir> becomes the resolution base for every
// relative path in the merged config (loops, instructions, router route,
// persistence, sqlite backend paths, shell key_file).
package config

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Trigger is one scheduled or event-driven task definition. Triggers are
// declarative data: cron expressions pass through as-is — the engine does not
// reduce them to timers; loops consume them via runtime.triggers().
type Trigger struct {
	Name      string         `yaml:"name"`
	Event     *TriggerEvent  `yaml:"event"` // kind = event
	Cron      string         `yaml:"cron"`  // kind = cron (expression passes through verbatim)
	Every     string         `yaml:"every"` // kind = every (duration, e.g. "15m")
	RunOnBoot bool           `yaml:"run_on_boot"`
	Target    TriggerTarget  `yaml:"target"`
	Payload   map[string]any `yaml:"payload"`
}

// TriggerEvent matches an inbound channel event.
type TriggerEvent struct {
	Channel string `yaml:"channel"`
	Match   string `yaml:"match"`
}

// TriggerTarget names the spawn profile a trigger runs as.
type TriggerTarget struct {
	Profile string `yaml:"profile"`
}

// agentFile is one profiles/*.yaml fragment: the existing AgentConfig plus a
// name and a spawn flag. spawn: true routes the file to
// profiles.agent (a spawn template); false (or absent) routes it to agents.
type agentFile struct {
	Name  string `yaml:"name"`
	Spawn bool   `yaml:"spawn"`
	Agent `yaml:",inline"`
}

// LoadDir loads and merges a config directory, validates the merged result,
// and rebases relative paths onto dir. See the package comment for the layout.
func LoadDir(dir string, log *slog.Logger) (*Config, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c := &Config{}

	// system.yaml is the only required fragment; it carries everything except
	// agents, channels, and triggers, which have their own homes.
	sysPath := filepath.Join(dir, "system.yaml")
	b, err := os.ReadFile(sysPath)
	if err != nil {
		return nil, fmt.Errorf("configdir %s: system.yaml: %w", dir, err)
	}
	if err := decodeStrictRaw(b, c, sysPath); err != nil {
		return nil, err
	}
	if len(c.Agents) > 0 {
		return nil, fmt.Errorf("configdir %s: agents belong in profiles/*.yaml, not system.yaml", dir)
	}
	if len(c.Gateway.Channels) > 0 {
		return nil, fmt.Errorf("configdir %s: channels belong in channels.yaml, not system.yaml", dir)
	}

	// channels.yaml -> gateway.channels.
	chPath := filepath.Join(dir, "channels.yaml")
	if b, err := os.ReadFile(chPath); err == nil {
		var frag struct {
			Channels []Channel `yaml:"channels"`
		}
		if err := decodeStrictRaw(b, &frag, chPath); err != nil {
			return nil, err
		}
		c.Gateway.Channels = frag.Channels
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("configdir %s: channels.yaml: %w", dir, err)
	}

	// profiles/*.yaml -> agents or profiles.agent, sorted-glob, keyed by name.
	c.Agents = map[string]Agent{}
	profileFiles, err := filepath.Glob(filepath.Join(dir, "profiles", "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("configdir %s: profiles: %w", dir, err)
	}
	sort.Strings(profileFiles)
	for _, pf := range profileFiles {
		b, err := os.ReadFile(pf)
		if err != nil {
			return nil, fmt.Errorf("configdir %s: %s: %w", dir, pf, err)
		}
		var f agentFile
		if err := decodeStrictRaw(b, &f, pf); err != nil {
			return nil, err
		}
		if f.Name == "" {
			return nil, fmt.Errorf("configdir %s: %s: profile has no name", dir, pf)
		}
		if f.Spawn {
			sp, err := f.spawnProfile()
			if err != nil {
				return nil, fmt.Errorf("configdir %s: %s: %w", dir, pf, err)
			}
			if c.Profiles.Agent == nil {
				c.Profiles.Agent = map[string]SpawnProfile{}
			}
			if _, dup := c.Profiles.Agent[f.Name]; dup {
				return nil, fmt.Errorf("configdir %s: duplicate spawn profile %q", dir, f.Name)
			}
			c.Profiles.Agent[f.Name] = sp
			continue
		}
		if _, dup := c.Agents[f.Name]; dup {
			return nil, fmt.Errorf("configdir %s: duplicate agent %q", dir, f.Name)
		}
		c.Agents[f.Name] = f.Agent
	}

	// triggers/*.yaml -> merged trigger list, sorted-glob; a trigger name
	// redefined by a later file wins (logged).
	triggerFiles, err := filepath.Glob(filepath.Join(dir, "triggers", "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("configdir %s: triggers: %w", dir, err)
	}
	sort.Strings(triggerFiles)
	triggerIdx := map[string]int{}
	for _, tf := range triggerFiles {
		b, err := os.ReadFile(tf)
		if err != nil {
			return nil, fmt.Errorf("configdir %s: %s: %w", dir, tf, err)
		}
		var frag struct {
			Triggers []Trigger `yaml:"triggers"`
		}
		if err := decodeStrictRaw(b, &frag, tf); err != nil {
			return nil, err
		}
		for _, tr := range frag.Triggers {
			if tr.Name == "" {
				return nil, fmt.Errorf("configdir %s: %s: trigger has no name", dir, tf)
			}
			if i, dup := triggerIdx[tr.Name]; dup {
				log.Warn("configdir: trigger overridden by later file",
					"trigger", tr.Name, "file", tf)
				c.Triggers[i] = tr
				continue
			}
			triggerIdx[tr.Name] = len(c.Triggers)
			c.Triggers = append(c.Triggers, tr)
		}
	}

	// Structured env expansion: every non-secret field expands ${VAR} now, as
	// the single-file path does; registry secret fields keep their raw
	// reference (${VAR} / cred:<service>) for lazy resolution at consumers.
	expandDeferredSecrets(c)

	if err := validate(dir, c); err != nil {
		return nil, err
	}
	// Rebase last so defaults applied by validate (runtime.persistence) pick
	// up the directory base too. Validation only checks references, not paths.
	rebaseConfigPaths(dir, c)
	return c, nil
}

// spawnProfile converts an agent file marked spawn: true into a SpawnProfile.
// Agent-only fields make no sense on a spawn template and are boot errors.
func (f *agentFile) spawnProfile() (SpawnProfile, error) {
	a := f.Agent
	if a.Persistent {
		return SpawnProfile{}, fmt.Errorf("spawn profile %q: persistent applies to agents, not spawn profiles", f.Name)
	}
	if a.Singleton {
		return SpawnProfile{}, fmt.Errorf("spawn profile %q: singleton applies to agents, not spawn profiles", f.Name)
	}
	if len(a.Taps) > 0 {
		return SpawnProfile{}, fmt.Errorf("spawn profile %q: taps apply to agents, not spawn profiles", f.Name)
	}
	if len(a.Channels) > 0 {
		return SpawnProfile{}, fmt.Errorf("spawn profile %q: channels apply to agents, not spawn profiles", f.Name)
	}
	if len(a.Lifecycle) > 0 {
		return SpawnProfile{}, fmt.Errorf("spawn profile %q: lifecycle applies to agents, not spawn profiles", f.Name)
	}
	if a.Memory.IsInline {
		return SpawnProfile{}, fmt.Errorf("spawn profile %q: spawn profiles reference memory profiles by name, not inline stores", f.Name)
	}
	sp := SpawnProfile{
		Model:        a.Model,
		Loop:         a.Loop,
		Instructions: a.Instructions,
		Memory:       a.Memory.Profile,
		Shell:        a.Shell,
		Skills:       a.Skills,
		Capabilities: a.Capabilities,
		CanContact:   a.CanContact,
	}
	if len(a.Budget) > 0 {
		sp.Budget = BudgetConfig{
			TokensPerDay: budgetMapInt(a.Budget, "tokens_per_day"),
			Window:       budgetMapString(a.Budget, "window"),
		}
	}
	return sp, nil
}

func budgetMapInt(m map[string]any, key string) int64 {
	switch n := m[key].(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

func budgetMapString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// rebaseConfigPaths makes dir the resolution base for every relative path in
// the merged config. Absolute paths and builtin: refs pass through untouched.
func rebaseConfigPaths(dir string, c *Config) {
	for name, a := range c.Agents {
		a.Loop = rebasePath(dir, a.Loop)
		a.Instructions = rebasePath(dir, a.Instructions)
		c.Agents[name] = a
	}
	for name, p := range c.Profiles.Agent {
		p.Loop = rebasePath(dir, p.Loop)
		p.Instructions = rebasePath(dir, p.Instructions)
		c.Profiles.Agent[name] = p
	}
	c.Gateway.Route = rebasePath(dir, c.Gateway.Route)
	c.Runtime.Persistence = rebasePersistence(dir, c.Runtime.Persistence)
	c.Runtime.Identity.Persistence = rebasePath(dir, c.Runtime.Identity.Persistence)
	c.Runtime.Credentials.Path = rebasePath(dir, c.Runtime.Credentials.Path)
	c.Media.Dir = rebasePath(dir, c.Media.Dir)
	c.Plugins.Dir = rebasePath(dir, c.Plugins.Dir)
	for name, b := range c.Memory.Backends {
		if b.Provider != "builtin:sqlite" || b.Config == nil {
			continue
		}
		if p, ok := b.Config["path"].(string); ok && p != "" {
			b.Config["path"] = rebasePath(dir, p)
			c.Memory.Backends[name] = b
		}
	}
	for name, p := range c.Profiles.Shell {
		p.KeyFile = rebasePath(dir, p.KeyFile)
		c.Profiles.Shell[name] = p
	}
}

func rebasePath(base, p string) string {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "builtin:") {
		return p
	}
	return filepath.Join(base, p)
}

// rebasePersistence rebases the path part of a sqlite:// reference, keeping
// the scheme prefix intact.
func rebasePersistence(base, p string) string {
	const prefix = "sqlite://"
	if rest, ok := strings.CutPrefix(p, prefix); ok {
		return prefix + rebasePath(base, rest)
	}
	return rebasePath(base, p)
}
