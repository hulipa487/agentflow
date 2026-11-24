package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var validAPINameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// APIConfig configures one External API extension package (Script/Goja or Wasm/wazero).
// Either Dir points to a directory containing manifest.yaml, or Runtime+Entry+Actions
// are declared inline.
type APIConfig struct {
	Dir          string                     `yaml:"dir,omitempty"`
	Version      string                     `yaml:"version,omitempty"`
	Description  string                     `yaml:"description,omitempty"`
	Runtime      string                     `yaml:"runtime,omitempty"` // script | wasm
	Entry        string                     `yaml:"entry,omitempty"`
	Timeout      string                     `yaml:"timeout,omitempty"`
	MemoryMB     int                        `yaml:"memory_mb,omitempty"`
	Capabilities APICapabilities            `yaml:"capabilities,omitempty"`
	Actions      map[string]APIActionConfig `yaml:"actions,omitempty"`
}

// APICapabilities declares host capabilities granted to an External API package.
type APICapabilities struct {
	HTTP *APIHTTPCapability `yaml:"http,omitempty"`
}

// APIHTTPCapability restricts outbound HTTP hosts and credentials for an API package.
type APIHTTPCapability struct {
	AllowedHosts []string `yaml:"allowed_hosts,omitempty"`
	Credentials  []string `yaml:"credentials,omitempty"`
}

// APIActionConfig describes an action exported by an inline External API config.
type APIActionConfig struct {
	Description        string         `yaml:"description,omitempty"`
	ExposeAsTool       bool           `yaml:"expose_as_tool,omitempty"`
	NeedsConfirm       bool           `yaml:"needs_confirm,omitempty"`
	Kind               string         `yaml:"kind,omitempty"` // read | write | destructive
	RequiresCapability string         `yaml:"requires_capability,omitempty"`
	Parameters         map[string]any `yaml:"parameters,omitempty"`
}

func validateAPIs(path string, c *Config) error {
	for name, a := range c.APIs {
		if !validAPINameRe.MatchString(name) {
			return fmt.Errorf("%s: apis: %q is not a valid api name (want 1..64 of A-Z a-z 0-9 _ . -)", path, name)
		}
		hasDir := strings.TrimSpace(a.Dir) != ""
		hasInline := strings.TrimSpace(a.Runtime) != "" || strings.TrimSpace(a.Entry) != ""
		if hasDir && hasInline {
			return fmt.Errorf("%s: apis.%s: dir cannot be combined with inline runtime or entry", path, name)
		}
		if !hasDir {
			switch strings.TrimSpace(a.Runtime) {
			case "script", "wasm":
			default:
				return fmt.Errorf("%s: apis.%s: runtime %q must be script or wasm (or set dir)", path, name, a.Runtime)
			}
			if strings.TrimSpace(a.Entry) == "" {
				return fmt.Errorf("%s: apis.%s: entry is required when dir is not set", path, name)
			}
			if len(a.Actions) == 0 {
				return fmt.Errorf("%s: apis.%s: actions must declare at least one action when dir is not set", path, name)
			}
		}
		if a.Timeout != "" {
			if d, err := time.ParseDuration(a.Timeout); err != nil || d <= 0 {
				return fmt.Errorf("%s: apis.%s: timeout %q must be a positive duration", path, name, a.Timeout)
			}
		}
		if a.MemoryMB < 0 || a.MemoryMB > 512 {
			return fmt.Errorf("%s: apis.%s: memory_mb %d out of range (0..512)", path, name, a.MemoryMB)
		}
		if a.Capabilities.HTTP != nil {
			for _, host := range a.Capabilities.HTTP.AllowedHosts {
				if strings.TrimSpace(host) == "" {
					return fmt.Errorf("%s: apis.%s: capabilities.http.allowed_hosts cannot contain blank entries", path, name)
				}
			}
			for _, cred := range a.Capabilities.HTTP.Credentials {
				if strings.TrimSpace(cred) == "" {
					return fmt.Errorf("%s: apis.%s: capabilities.http.credentials cannot contain blank entries", path, name)
				}
			}
		}
		for act, actCfg := range a.Actions {
			if !validAPINameRe.MatchString(act) {
				return fmt.Errorf("%s: apis.%s: invalid action name %q", path, name, act)
			}
			switch strings.ToLower(strings.TrimSpace(actCfg.Kind)) {
			case "", "read", "write", "destructive":
			default:
				return fmt.Errorf("%s: apis.%s.actions.%s: kind must be read, write or destructive (got %q)", path, name, act, actCfg.Kind)
			}
		}
	}
	return nil
}
