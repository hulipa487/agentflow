package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSourceAutoDetectsConfigDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "system.yaml"), []byte("version: \"1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(orig) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	src, err := ResolveSource("")
	if err != nil {
		t.Fatalf("ResolveSource(\"\") failed in configdir: %v", err)
	}
	if !src.IsDir {
		t.Fatalf("expected IsDir=true, got %+v", src)
	}
}

func TestValidateAgentConversationalStillChecksCanContactAndSkills(t *testing.T) {
	cfg := &Config{
		Agents: map[string]Agent{
			"a": {
				Loop:         "./a.lua",
				Memory:       MemoryAgentConfig{Profile: "conversational"},
				Capabilities: []string{"llm.chat", "memory"},
				CanContact:   []string{"nonexistent"},
			},
		},
	}
	err := cfg.ValidateAgent("a", cfg.Agents["a"], "test.yaml")
	if err == nil || !strings.Contains(err.Error(), "can_contact") {
		t.Fatalf("expected can_contact error for conversational agent, got: %v", err)
	}
}

func TestVMMemoryLimitValidatedAndSupportsMiB(t *testing.T) {
	if _, err := loadTestConfigYAML(t, `
version: "1"
runtime:
  vm:
    memory_limit: 64MiB
`); err != nil {
		t.Fatalf("expected 64MiB memory_limit to be accepted, got: %v", err)
	}
	if _, err := loadTestConfigYAML(t, `
version: "1"
runtime:
  vm:
    memory_limit: not-a-size
`); err == nil || !strings.Contains(err.Error(), "runtime.vm.memory_limit") {
		t.Fatalf("expected memory_limit validation error, got: %v", err)
	}
}

func TestRegionalSharedMediaFilesAllowed(t *testing.T) {
	_, err := loadTestConfigYAML(t, `
version: "1"
runtime:
  topology: regional
  region: us-east
  persistence: postgres://u:p@db.internal:5432/agentflow
media:
  backend: s3
  s3:
    bucket: shared-blobs
    region: us-east-1
    access_key: ak
    secret_key: sk
`)
	if err != nil {
		t.Fatalf("expected regional config with shared media/files S3 store to validate, got: %v", err)
	}
}

func TestConfigDirRebasesAllRelativePathsAndPreservesSpawnFields(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "system.yaml"), []byte(`
version: "1"
runtime:
  studio:
    workspace: ./studio-ws
  sandbox:
    data_dir: ./sbx
files:
  dir: ./my-files
environments:
  py:
    rootfs: ./rootfs/py
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profiles", "worker.yaml"), []byte(`
name: worker
spawn: true
loop: ./loops/worker.lua
history_budget: 12000
inherit_user: false
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadDir(dir, nil)
	if err != nil {
		t.Fatalf("LoadDir failed: %v", err)
	}
	if !filepath.IsAbs(cfg.Files.Dir) || !strings.HasSuffix(filepath.ToSlash(cfg.Files.Dir), "/my-files") {
		t.Fatalf("files.dir not rebased: %q", cfg.Files.Dir)
	}
	if !filepath.IsAbs(cfg.Runtime.Studio.Workspace) {
		t.Fatalf("runtime.studio.workspace not rebased: %q", cfg.Runtime.Studio.Workspace)
	}
	if !filepath.IsAbs(cfg.Runtime.Sandbox.DataDir) {
		t.Fatalf("runtime.sandbox.data_dir not rebased: %q", cfg.Runtime.Sandbox.DataDir)
	}
	if !filepath.IsAbs(cfg.Environments["py"].Rootfs) {
		t.Fatalf("environments.py.rootfs not rebased: %q", cfg.Environments["py"].Rootfs)
	}
	sp, ok := cfg.Profiles.Agent["worker"]
	if !ok {
		t.Fatal("missing spawn profile worker")
	}
	if sp.HistoryBudget != 12000 {
		t.Fatalf("spawn profile HistoryBudget = %d, want 12000", sp.HistoryBudget)
	}
	if sp.InheritUser == nil || *sp.InheritUser != false {
		t.Fatalf("spawn profile InheritUser = %v, want false", sp.InheritUser)
	}
}
