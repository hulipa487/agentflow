package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseByteSize(t *testing.T) {
	good := map[string]int64{
		"1048576": 1 << 20, "512m": 512 << 20, "512MB": 512 << 20, "512MiB": 512 << 20,
		"2g": 2 << 30, "1.5GiB": 3 << 29, "256KiB": 256 << 10, "64 kb": 64 << 10, "10b": 10,
	}
	for in, want := range good {
		if got, err := ParseByteSize(in); err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "lots", "-1m", "5x", "1e9", "9999999999t"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Errorf("ParseByteSize(%q) was accepted", bad)
		}
	}
}

func TestSandboxDefaults(t *testing.T) {
	var s SandboxConfig
	if s.QueueWait() != 30*time.Second || s.JobTimeout() != time.Minute || s.JobMaxTimeout() != 10*time.Minute || s.BackgroundTimeout() != 2*time.Hour {
		t.Errorf("default durations: %s %s %s %s", s.QueueWait(), s.JobTimeout(), s.JobMaxTimeout(), s.BackgroundTimeout())
	}
	if s.OutputBytes() != 256<<10 || s.TmpBytes() != 256<<20 || s.MemoryBytes() != 1<<30 || s.WriteBackBytes() != 1<<30 || s.WriteBackFiles() != 10000 {
		t.Error("default sizes are not the documented ones")
	}
	s.Timeout, s.Memory, s.WriteBack.MaxFiles = "5s", "64m", 7
	if s.JobTimeout() != 5*time.Second || s.MemoryBytes() != 64<<20 || s.WriteBackFiles() != 7 {
		t.Error("explicit settings were not applied")
	}
	c := &Config{}
	c.Runtime.Persistence = "sqlite://./var/agentflow.db"
	if got := c.SandboxDataDir(); got != "var/sandbox" {
		t.Errorf("SandboxDataDir = %q, want it beside the runtime store", got)
	}
	c.Runtime.Sandbox.DataDir = "/srv/af"
	if got := c.SandboxDataDir(); got != "/srv/af" {
		t.Errorf("SandboxDataDir = %q", got)
	}
}

func TestValidateSandboxAndEnvironments(t *testing.T) {
	pin := "python:3.12-slim@sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name string
		edit func(*Config)
		want string // "" = valid
	}{
		{"empty is valid", func(*Config) {}, ""},
		{"a pinned image", func(c *Config) { c.Environments = map[string]Environment{"py": {Image: pin}} }, ""},
		{"a local rootfs", func(c *Config) { c.Environments = map[string]Environment{"py": {Rootfs: "/srv/py"}} }, ""},
		{"a tag is not a pin", func(c *Config) { c.Environments = map[string]Environment{"py": {Image: "python:3.12"}} }, "must be pinned by digest"},
		{"image and rootfs", func(c *Config) { c.Environments = map[string]Environment{"py": {Image: pin, Rootfs: "/x"}} }, "exactly one of image"},
		{"neither", func(c *Config) { c.Environments = map[string]Environment{"py": {}} }, "exactly one of image"},
		{"bad name", func(c *Config) { c.Environments = map[string]Environment{"a b": {Rootfs: "/x"}} }, "not a usable name"},
		{"bad variable", func(c *Config) {
			c.Environments = map[string]Environment{"py": {Rootfs: "/x", Env: map[string]string{"A=B": "1"}}}
		}, "not a usable variable name"},
		{"bad duration", func(c *Config) { c.Runtime.Sandbox.QueueTimeout = "soon" }, "runtime.sandbox.queue_timeout"},
		{"bad size", func(c *Config) { c.Runtime.Sandbox.Memory = "lots" }, "runtime.sandbox.memory"},
		{"timeout over its cap", func(c *Config) { c.Runtime.Sandbox.Timeout, c.Runtime.Sandbox.MaxTimeout = "20m", "5m" }, "longer than max_timeout"},
		{"negative count", func(c *Config) { c.Runtime.Sandbox.Concurrency = -1 }, "cannot be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{}
			tc.edit(c)
			err := validateSandbox("cfg.yaml", c)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("validateSandbox = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}
