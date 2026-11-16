package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// Source is a resolved configuration source: either one agentflow.yaml file or
// a config directory (system.yaml + profiles/*.yaml + channels.yaml).
type Source struct {
	Path  string // absolute file path; empty for a directory
	Dir   string // absolute base directory (the file's directory, or the config dir)
	IsDir bool
	// Legacy is true when discovery picked config.yaml instead of
	// agentflow.yaml. The caller warns: the fallback exists for old
	// checkouts, not as a second documented name.
	Legacy bool
}

// String renders the source the way operators passed it.
func (s Source) String() string {
	if s.IsDir {
		return s.Dir
	}
	return s.Path
}

// ConfigDir is the directory for consumers that only understand the
// config-directory layout (empty for a single-file config).
func (s Source) ConfigDir() string {
	if s.IsDir {
		return s.Dir
	}
	return ""
}

// ConfigPath is the single-file path for consumers that only understand one
// file (empty for a directory config).
func (s Source) ConfigPath() string {
	if s.IsDir {
		return ""
	}
	return s.Path
}

// Load reads the resolved source.
func (s Source) Load(log *slog.Logger) (*Config, error) {
	if s.IsDir {
		return LoadDir(s.Dir, log)
	}
	return Load(s.Path)
}

// ResolveSource turns -config into one absolute source. The path may name a
// file, a config directory (system.yaml + profiles/), or a directory holding an
// agentflow.yaml. With no path it discovers agentflow.yaml and then config.yaml
// in the working directory, and reports when it used the legacy name. The
// engine and every CLI command use this function, so they cannot disagree
// about what "the config" is.
func ResolveSource(path string) (Source, error) {
	if path != "" {
		return resolvePath(path, false)
	}
	for _, candidate := range []string{"agentflow.yaml", "config.yaml"} {
		if _, err := os.Stat(candidate); err == nil {
			return resolvePath(candidate, candidate != "agentflow.yaml")
		} else if !os.IsNotExist(err) {
			return Source{}, err
		}
	}
	return Source{}, fmt.Errorf("no config found: looked for agentflow.yaml and config.yaml in %s; run `agentflow init` or pass -config", mustGetwd())
}

func resolvePath(path string, legacy bool) (Source, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Source{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Source{}, fmt.Errorf("config %s: %w", abs, err)
	}
	if info.IsDir() {
		// `init DIR` writes DIR/agentflow.yaml, so -config DIR has to find it.
		// A directory with a system.yaml is a config directory either way.
		if !fileExists(filepath.Join(abs, "system.yaml")) && fileExists(filepath.Join(abs, "agentflow.yaml")) {
			file := filepath.Join(abs, "agentflow.yaml")
			return Source{Path: file, Dir: abs}, nil
		}
		return Source{Dir: abs, IsDir: true}, nil
	}
	return Source{Path: abs, Dir: filepath.Dir(abs), Legacy: legacy}, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "the working directory"
	}
	return wd
}
