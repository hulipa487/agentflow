package config

import (
	"strings"
	"testing"
	"time"
)

// regionalConfig is a configuration every regional rule accepts: one PostgreSQL
// target, S3 for both blob stores, no SQLite memory backend and no local log
// plane. Each refusal test mutates exactly one thing.
func regionalConfig() *Config {
	return &Config{
		Agents: map[string]Agent{"bot": {Loop: "plugin:per_chat"}},
		Runtime: Runtime{
			Persistence: "postgres://agentflow:secret@db.internal:5432/agentflow",
			Topology:    "regional",
			Region:      "eu-west",
			Zone:        "eu-west-1a",
		},
		Media: MediaConfig{
			Backend: "s3",
			S3:      MediaS3{Bucket: "agentflow-media", Region: "eu-west-1"},
		},
		Files: FilesConfig{
			Backend: "s3",
			S3:      MediaS3{Bucket: "agentflow-files", Region: "eu-west-1"},
		},
	}
}

func TestTopologyDefaultsToSingle(t *testing.T) {
	c := &Config{}
	if got := c.Topology(); got != "single" {
		t.Fatalf("unset topology = %q, want single", got)
	}
	if c.IsRegional() {
		t.Fatal("unset topology reported regional")
	}
	c.Runtime.Topology = "REGIONAL"
	if got := c.Topology(); got != "regional" {
		t.Fatalf("Topology() = %q, want regional (case-insensitive)", got)
	}
	if !c.IsRegional() {
		t.Fatal("regional topology not reported")
	}
}

func TestValidateTopologyValue(t *testing.T) {
	c := &Config{Runtime: Runtime{Topology: "fleet"}}
	err := validate("test.yaml", c)
	if err == nil || !strings.Contains(err.Error(), "runtime.topology") {
		t.Fatalf("err = %v, want a runtime.topology refusal", err)
	}
}

func TestValidateRuntimeZone(t *testing.T) {
	c := &Config{Runtime: Runtime{Zone: "eu west/1a"}}
	err := validate("test.yaml", c)
	if err == nil || !strings.Contains(err.Error(), "runtime.zone") {
		t.Fatalf("err = %v, want a runtime.zone refusal", err)
	}
}

func TestValidateRegionalAcceptance(t *testing.T) {
	if err := validate("test.yaml", regionalConfig()); err != nil {
		t.Fatalf("valid regional config refused: %v", err)
	}
}

func TestValidateRegionalRefusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name:   "sqlite runtime store",
			mutate: func(c *Config) { c.Runtime.Persistence = "sqlite://./data/agentflow.db" },
			want:   "runtime.persistence is not a PostgreSQL target",
		},
		{
			name:   "sqlite identity store",
			mutate: func(c *Config) { c.Runtime.Identity.Persistence = "sqlite://./data/identity.db" },
			want:   "runtime.identity.persistence is a SQLite target",
		},
		{
			name:   "sqlite credentials store",
			mutate: func(c *Config) { c.Runtime.Credentials.Path = "credentials.db" },
			want:   "runtime.credentials.path is a SQLite target",
		},
		{
			name: "cluster store differs",
			mutate: func(c *Config) {
				c.Runtime.Cluster.Persistence = "postgres://agentflow:secret@other.internal:5432/agentflow"
			},
			want: "runtime.cluster.persistence",
		},
		{
			name: "sqlite memory backend",
			mutate: func(c *Config) {
				c.Memory.Backends = map[string]Backend{"cache": {Provider: "sqlite"}}
			},
			want: `memory backend "cache" uses provider sqlite`,
		},
		{
			name:   "filesystem media",
			mutate: func(c *Config) { c.Media.Backend = "fs" },
			want:   "media.backend",
		},
		{
			name:   "filesystem files",
			mutate: func(c *Config) { c.Files.Backend = "fs" },
			want:   "files.backend",
		},
		{
			name:   "local log plane",
			mutate: func(c *Config) { c.Runtime.LogPlane.Persistence = "./data/logs" },
			want:   "runtime.log_plane.persistence",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := regionalConfig()
			tt.mutate(c)
			err := validate("test.yaml", c)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want a refusal containing %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "regional topology") {
				t.Fatalf("err = %v, want the refusal to name the topology rule", err)
			}
		})
	}
}

// TestRegionalClusterStoreSameTarget: the deprecated setting is tolerated when
// it names the runtime store, even under the other PostgreSQL scheme.
func TestRegionalClusterStoreSameTarget(t *testing.T) {
	c := regionalConfig()
	c.Runtime.Cluster.Persistence = "postgresql://agentflow:secret@db.internal:5432/agentflow"
	if err := validate("test.yaml", c); err != nil {
		t.Fatalf("cluster.persistence naming the runtime store was refused: %v", err)
	}
}

func TestSameStoreTarget(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"postgres://u:p@db:5432/agent", "postgresql://other:secret@db:5432/agent", true},
		{"postgres://u:p@db/agent", "postgres://u:p@db:5432/agent", true},
		{"postgres://u:p@DB:5432/agent", "postgres://u:p@db:5432/agent", true},
		{"postgres://u:p@db:5432/agent", "postgres://u:p@db:5432/other", false},
		{"postgres://u:p@db1:5432/agent", "postgres://u:p@db2:5432/agent", false},
		{"sqlite://./data/a.db", "postgres://u:p@db:5432/agent", false},
		{"sqlite://./data/a.db", "./data/a.db", true},
	}
	for _, tt := range tests {
		if got := SameStoreTarget(tt.a, tt.b); got != tt.want {
			t.Errorf("SameStoreTarget(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestSamePostgresServer(t *testing.T) {
	if !SamePostgresServer("postgres://u:p@db:5432/a", "postgres://u:p@db:5432/b") {
		t.Fatal("same host and port should be one server")
	}
	if SamePostgresServer("postgres://u:p@db:5432/a", "postgres://u:p@db:5433/a") {
		t.Fatal("different ports should not be one server")
	}
	if SamePostgresServer("sqlite://./data/a.db", "sqlite://./data/a.db") {
		t.Fatal("a SQLite target has no PostgreSQL server")
	}
}

func TestShutdownGraceDefault(t *testing.T) {
	if got := (&Config{}).ShutdownGrace(); got != 0 {
		t.Fatalf("single default grace = %v, want 0", got)
	}
	if got := (&Config{Runtime: Runtime{Topology: "regional"}}).ShutdownGrace(); got != 5*time.Second {
		t.Fatalf("regional default grace = %v, want 5s", got)
	}
	for raw, want := range map[string]time.Duration{
		"0":   0,
		"2s":  2 * time.Second,
		"1m":  time.Minute,
		"30s": 30 * time.Second,
	} {
		c := &Config{Runtime: Runtime{ShutdownGrace: raw}}
		if got := c.ShutdownGrace(); got != want {
			t.Errorf("shutdown_grace %q = %v, want %v", raw, got, want)
		}
	}
	c := &Config{Runtime: Runtime{ShutdownGrace: "-1s"}}
	if err := validate("test.yaml", c); err == nil || !strings.Contains(err.Error(), "runtime.shutdown_grace") {
		t.Fatalf("negative grace err = %v, want a runtime.shutdown_grace refusal", err)
	}
}
