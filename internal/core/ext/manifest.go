package ext

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RuntimeKind selects the sandbox used to execute an External API package.
type RuntimeKind string

const (
	RuntimeScript RuntimeKind = "script"
	RuntimeWasm   RuntimeKind = "wasm"
)

const (
	DefaultTimeout  = 15 * time.Second
	DefaultMemoryMB = 16
	MaxMemoryMB     = 512
)

var validNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Manifest describes an External API package (TypeScript/JS on Goja or Wasm on wazero).
type Manifest struct {
	Name         string                `yaml:"name" json:"name"`
	Version      string                `yaml:"version,omitempty" json:"version,omitempty"`
	Description  string                `yaml:"description,omitempty" json:"description,omitempty"`
	Runtime      RuntimeKind           `yaml:"runtime" json:"runtime"`
	Entry        string                `yaml:"entry" json:"entry"`
	Timeout      string                `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	MemoryMB     int                   `yaml:"memory_mb,omitempty" json:"memory_mb,omitempty"`
	Capabilities Capabilities          `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Actions      map[string]ActionSpec `yaml:"actions" json:"actions"`
}

// Capabilities lists the host resources an External API is permitted to touch.
type Capabilities struct {
	HTTP *HTTPCapability `yaml:"http,omitempty" json:"http,omitempty"`
}

// HTTPCapability bounds host-proxied outbound HTTP calls for this extension.
type HTTPCapability struct {
	AllowedHosts []string `yaml:"allowed_hosts" json:"allowed_hosts"`
	Credentials  []string `yaml:"credentials,omitempty" json:"credentials,omitempty"`
}

// ActionSpec describes one callable entry point exported by an External API.
type ActionSpec struct {
	Description        string         `yaml:"description,omitempty" json:"description,omitempty"`
	ExposeAsTool       bool           `yaml:"expose_as_tool,omitempty" json:"expose_as_tool,omitempty"`
	NeedsConfirm       bool           `yaml:"needs_confirm,omitempty" json:"needs_confirm,omitempty"`
	RequiresCapability string         `yaml:"requires_capability,omitempty" json:"requires_capability,omitempty"`
	Parameters         map[string]any `yaml:"parameters,omitempty" json:"parameters,omitempty"`
}

// LoadManifestDir reads and validates <dir>/manifest.yaml.
func LoadManifestDir(dir string) (Manifest, error) {
	path := filepath.Join(dir, "manifest.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read %s: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if m.Name == "" {
		m.Name = filepath.Base(filepath.Clean(dir))
	}
	if err := ValidateManifest(m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := ResolveEntryPath(dir, m.Entry, true); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// ValidateManifest checks required fields, runtime choice, capability bounds, and actions.
func ValidateManifest(m Manifest) error {
	if !validNameRe.MatchString(m.Name) {
		return fmt.Errorf("invalid api name %q (want 1..64 of A-Z a-z 0-9 _ . -)", m.Name)
	}
	switch m.Runtime {
	case RuntimeScript, RuntimeWasm:
	default:
		return fmt.Errorf("api %q: unsupported runtime %q (want %q or %q)", m.Name, m.Runtime, RuntimeScript, RuntimeWasm)
	}
	if strings.TrimSpace(m.Entry) == "" {
		return fmt.Errorf("api %q: entry is required", m.Name)
	}
	if m.Timeout != "" {
		d, err := time.ParseDuration(m.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("api %q: timeout %q must be a positive duration", m.Name, m.Timeout)
		}
	}
	if m.MemoryMB < 0 || m.MemoryMB > MaxMemoryMB {
		return fmt.Errorf("api %q: memory_mb %d out of range (0..%d)", m.Name, m.MemoryMB, MaxMemoryMB)
	}
	if m.Capabilities.HTTP != nil {
		for _, host := range m.Capabilities.HTTP.AllowedHosts {
			if strings.TrimSpace(host) == "" {
				return fmt.Errorf("api %q: capabilities.http.allowed_hosts cannot contain blank entries", m.Name)
			}
		}
		for _, cred := range m.Capabilities.HTTP.Credentials {
			if strings.TrimSpace(cred) == "" {
				return fmt.Errorf("api %q: capabilities.http.credentials cannot contain blank entries", m.Name)
			}
		}
	}
	if len(m.Actions) == 0 {
		return fmt.Errorf("api %q: actions must declare at least one action", m.Name)
	}
	for actName := range m.Actions {
		if !validNameRe.MatchString(actName) {
			return fmt.Errorf("api %q: invalid action name %q", m.Name, actName)
		}
	}
	return nil
}

// TimeoutDuration returns the configured timeout or DefaultTimeout.
func (m Manifest) TimeoutDuration() time.Duration {
	if m.Timeout == "" {
		return DefaultTimeout
	}
	d, err := time.ParseDuration(m.Timeout)
	if err != nil || d <= 0 {
		return DefaultTimeout
	}
	return d
}

// EffectiveMemoryPages returns the 64 KiB Wasm memory page ceiling for m.
func (m Manifest) EffectiveMemoryPages() uint32 {
	mb := m.MemoryMB
	if mb <= 0 {
		mb = DefaultMemoryMB
	}
	return uint32(mb) * 16 // 1 MiB = 16 x 64 KiB Wasm pages
}

// ResolveEntryPath resolves entry relative to baseDir and, when confine is true,
// rejects any path escaping baseDir.
func ResolveEntryPath(baseDir, entry string, confine bool) (string, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", fmt.Errorf("entry is required")
	}
	if filepath.IsAbs(entry) {
		if confine {
			return "", fmt.Errorf("entry %q must be relative to package directory", entry)
		}
		return filepath.Clean(entry), nil
	}
	if baseDir == "" {
		return filepath.Clean(entry), nil
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(filepath.Join(absBase, entry))
	if err != nil {
		return "", err
	}
	if confine {
		rel, err := filepath.Rel(absBase, absTarget)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return "", fmt.Errorf("entry %q escapes package directory", entry)
		}
	}
	return absTarget, nil
}
