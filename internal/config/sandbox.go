package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SandboxConfig is runtime.sandbox: how shell jobs run on this instance. Every
// field has a default, so a deployment that only declares an environment and a
// shell profile needs none of it.
type SandboxConfig struct {
	// Provider selects the sandbox backend: "gvisor" (default, Linux runsc) or
	// "microsandbox" (superradcompany/microsandbox msb CLI over KVM/HVF/WHP).
	Provider string `yaml:"provider"`
	// Runsc is the gVisor runtime binary; empty means "runsc" on PATH.
	Runsc string `yaml:"runsc"`
	// Microsandbox is the microsandbox CLI binary; empty means "msb" on PATH.
	Microsandbox string `yaml:"microsandbox"`
	// DataDir holds workspaces, unpacked images and runsc's own state; empty
	// means <data dir>/sandbox. It is a cache: everything in it is rebuilt
	// from the files store and the registry.
	DataDir string `yaml:"data_dir"`
	// Concurrency is how many jobs run at once on this instance (default 4).
	Concurrency int `yaml:"concurrency"`
	// PerUser is how many of those one user may hold (default 2).
	PerUser int `yaml:"per_user"`
	// QueueTimeout is how long a job waits for its turn (default 30s).
	QueueTimeout string `yaml:"queue_timeout"`
	// Timeout is a job's time limit when nothing else sets one (default 60s).
	Timeout string `yaml:"timeout"`
	// MaxTimeout caps what a call, a project or a profile may ask for
	// (default 10m).
	MaxTimeout string `yaml:"max_timeout"`
	// BackgroundMaxTimeout is the cap for shell.start jobs (default 2h).
	BackgroundMaxTimeout string `yaml:"background_max_timeout"`
	// OutputLimit caps each of stdout and stderr (default 256KiB).
	OutputLimit string `yaml:"output_limit"`
	// TmpLimit is the size of a job's /tmp and home directory (default 256MiB).
	TmpLimit string `yaml:"tmp_limit"`
	// Memory bounds a whole sandbox (default 1GiB). It is enforced where the
	// host allows; `agentflow doctor` says whether it is.
	Memory string `yaml:"memory"`
	// CPUs bounds a whole sandbox in cores (default: no limit).
	CPUs float64 `yaml:"cpus"`
	// PIDs bounds the processes inside a sandbox (default 512).
	PIDs int `yaml:"pids"`
	// WriteBack bounds what one job may store in its project.
	WriteBack struct {
		MaxFiles int    `yaml:"max_files"` // default 10000
		MaxBytes string `yaml:"max_bytes"` // default 1GiB
	} `yaml:"write_back"`
}

// Environment is one entry of environments: a named, declared toolchain for
// shell jobs. Exactly one of Image and Rootfs is set.
type Environment struct {
	// Image is an OCI image pinned by digest (name@sha256:…). It is pulled
	// the first time the environment is used on an instance.
	Image string `yaml:"image"`
	// Rootfs is a directory on the host that already holds a root filesystem.
	Rootfs string `yaml:"rootfs"`
	// Env is set for every job, over what the image declares.
	Env map[string]string `yaml:"env"`
}

// SandboxDataDir is where shell jobs keep their on-disk cache.
func (c *Config) SandboxDataDir() string {
	if d := strings.TrimSpace(c.Runtime.Sandbox.DataDir); d != "" {
		return d
	}
	return filepath.Join(c.DataDir(), "sandbox")
}

// ProviderName returns the configured sandbox provider ("gvisor" or
// "microsandbox"), defaulting to "gvisor".
func (s SandboxConfig) ProviderName() string {
	p := strings.ToLower(strings.TrimSpace(s.Provider))
	if p == "" {
		return "gvisor"
	}
	return p
}

// The accessors below return a setting with its default applied. Validation
// has already parsed every value once, so they cannot fail here.

func (s SandboxConfig) duration(v string, def time.Duration) time.Duration {
	if strings.TrimSpace(v) == "" {
		return def
	}
	d, _ := time.ParseDuration(v)
	return d
}

func (s SandboxConfig) QueueWait() time.Duration  { return s.duration(s.QueueTimeout, 30*time.Second) }
func (s SandboxConfig) JobTimeout() time.Duration { return s.duration(s.Timeout, 60*time.Second) }
func (s SandboxConfig) JobMaxTimeout() time.Duration {
	return s.duration(s.MaxTimeout, 10*time.Minute)
}
func (s SandboxConfig) BackgroundTimeout() time.Duration {
	return s.duration(s.BackgroundMaxTimeout, 2*time.Hour)
}

func (s SandboxConfig) bytes(v string, def int64) int64 {
	if strings.TrimSpace(v) == "" {
		return def
	}
	n, _ := ParseByteSize(v)
	return n
}

func (s SandboxConfig) OutputBytes() int64    { return s.bytes(s.OutputLimit, 256<<10) }
func (s SandboxConfig) TmpBytes() int64       { return s.bytes(s.TmpLimit, 256<<20) }
func (s SandboxConfig) MemoryBytes() int64    { return s.bytes(s.Memory, 1<<30) }
func (s SandboxConfig) WriteBackBytes() int64 { return s.bytes(s.WriteBack.MaxBytes, 1<<30) }
func (s SandboxConfig) WriteBackFiles() int {
	if s.WriteBack.MaxFiles > 0 {
		return s.WriteBack.MaxFiles
	}
	return 10000
}

var byteSizeRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([kmgt]?)(i?b)?$`)

// ParseByteSize reads a size: a byte count, or a number with a unit. Units are
// binary and case-insensitive, and every usual spelling of one means the same
// thing — 512m, 512mb and 512MiB are all 512 × 2²⁰ — because that is what
// people mean when they give a container a memory limit.
func ParseByteSize(v string) (int64, error) {
	m := byteSizeRE.FindStringSubmatch(strings.ToLower(strings.TrimSpace(v)))
	if m == nil {
		return 0, fmt.Errorf("%q is not a size (use a byte count or a unit, such as 512MiB or 2g)", v)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size", v)
	}
	shift := map[string]uint{"": 0, "k": 10, "m": 20, "g": 30, "t": 40}[m[2]]
	out := n * float64(int64(1)<<shift)
	if out > float64(int64(1)<<62) {
		return 0, fmt.Errorf("%q is too large", v)
	}
	return int64(out), nil
}

var imagePinRE = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

// validateSandbox checks runtime.sandbox and environments. It runs whether or
// not any agent uses a shell: a typo in a limit should be found at load, not
// by the first command somebody runs.
func validateSandbox(path string, c *Config) error {
	s := c.Runtime.Sandbox
	switch s.ProviderName() {
	case "gvisor", "microsandbox":
	default:
		return fmt.Errorf("%s: runtime.sandbox.provider %q is unknown (use gvisor or microsandbox)", path, s.Provider)
	}
	for name, v := range map[string]string{
		"queue_timeout": s.QueueTimeout, "timeout": s.Timeout,
		"max_timeout": s.MaxTimeout, "background_max_timeout": s.BackgroundMaxTimeout,
	} {
		if strings.TrimSpace(v) == "" {
			continue
		}
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			return fmt.Errorf("%s: runtime.sandbox.%s %q is not a positive duration (such as 30s or 10m)", path, name, v)
		}
	}
	for name, v := range map[string]string{
		"output_limit": s.OutputLimit, "tmp_limit": s.TmpLimit,
		"memory": s.Memory, "write_back.max_bytes": s.WriteBack.MaxBytes,
	} {
		if strings.TrimSpace(v) == "" {
			continue
		}
		if n, err := ParseByteSize(v); err != nil || n <= 0 {
			return fmt.Errorf("%s: runtime.sandbox.%s: %q is not a positive size (such as 256KiB or 1GiB)", path, name, v)
		}
	}
	if s.JobTimeout() > s.JobMaxTimeout() {
		return fmt.Errorf("%s: runtime.sandbox.timeout (%s) is longer than max_timeout (%s)", path, s.JobTimeout(), s.JobMaxTimeout())
	}
	if s.Concurrency < 0 || s.PerUser < 0 || s.PIDs < 0 || s.CPUs < 0 || s.WriteBack.MaxFiles < 0 {
		return fmt.Errorf("%s: runtime.sandbox: concurrency, per_user, pids, cpus and write_back.max_files cannot be negative", path)
	}

	for name, e := range c.Environments {
		image, rootfs := strings.TrimSpace(e.Image), strings.TrimSpace(e.Rootfs)
		switch {
		case name == "" || strings.ContainsAny(name, " \t/\\|"):
			return fmt.Errorf("%s: environments: %q is not a usable name", path, name)
		case (image == "") == (rootfs == ""):
			return fmt.Errorf("%s: environment %q needs exactly one of image (a pinned OCI image) and rootfs (a directory on this host)", path, name)
		case image != "" && !imagePinRE.MatchString(image):
			return fmt.Errorf("%s: environment %q: image %q must be pinned by digest (name@sha256:…): a tag can be moved to different content, and then two instances would run different toolchains under one name", path, name, e.Image)
		}
		for k := range e.Env {
			if k == "" || strings.ContainsAny(k, "=\x00") {
				return fmt.Errorf("%s: environment %q: %q is not a usable variable name", path, name, k)
			}
		}
	}
	return nil
}

// EnvironmentNames lists the declared environments, sorted.
func (c *Config) EnvironmentNames() []string {
	out := make([]string, 0, len(c.Environments))
	for name := range c.Environments {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// validateShellProfile checks a shell profile referenced by an agent or a
// spawn profile.
func validateShellProfile(path, owner, name string, p ShellProfile, c *Config) error {
	where := fmt.Sprintf("%s: shell profile %q (used by %q)", path, name, owner)
	switch p.Provider {
	case "ssh":
		if p.Host == "" {
			return fmt.Errorf("%s is provider ssh but missing required host", where)
		}
		return nil
	case "sandbox", "":
	default:
		return fmt.Errorf("%s has unknown provider %q (use sandbox or ssh)", where, p.Provider)
	}
	if p.Environment == "" {
		return fmt.Errorf("%s names no environment (declared: %s)", where, strings.Join(c.EnvironmentNames(), ", "))
	}
	if _, ok := c.Environments[p.Environment]; !ok {
		return fmt.Errorf("%s names environment %q, which is not declared (declared: %s)", where, p.Environment, strings.Join(c.EnvironmentNames(), ", "))
	}
	if p.Network != "" && p.Network != "none" {
		return fmt.Errorf("%s sets network %q: sandbox jobs have no network yet, so the only value is none", where, p.Network)
	}
	if p.Timeout != "" {
		if d, err := time.ParseDuration(p.Timeout); err != nil || d <= 0 {
			return fmt.Errorf("%s has timeout %q, which is not a positive duration (such as 90s)", where, p.Timeout)
		}
	}
	if p.MemLimit != "" {
		if n, err := ParseByteSize(p.MemLimit); err != nil || n <= 0 {
			return fmt.Errorf("%s has mem_limit %q, which is not a positive size (such as 512MiB)", where, p.MemLimit)
		}
	}
	if p.Project != "" && strings.ContainsAny(p.Project, "|/\\\x00") {
		return fmt.Errorf("%s has project %q, which is not a usable project name", where, p.Project)
	}
	return nil
}
