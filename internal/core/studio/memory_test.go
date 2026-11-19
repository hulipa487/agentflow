package studio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"agentflow/internal/config"
)

func memoryFixture(t *testing.T, system string) (*Memory, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "system.yaml"), system)
	writeFile(t, filepath.Join(dir, "profiles", "chat.yaml"), "name: chat\nloop: ./chat.lua\nsafety: none\nmemory: conversational\n")
	cfg, err := config.LoadDir(dir, safetyLog())
	if err != nil {
		t.Fatal(err)
	}
	return NewMemory(cfg, dir, "", safetyLog()), dir
}

const memorySystem = `
version: "1"
models:
  default: { provider: openai, model: gpt-4o-mini, base_url: http://127.0.0.1:11434/v1 }
  embed: { provider: openai, model: text-embedding-3-small, base_url: http://127.0.0.1:11434/v1 }
profiles:
  safety:
    none: { filters: [] }
`

func findBackend(t *testing.T, l MemoryList, name string) MemoryBackendView {
	t.Helper()
	for _, b := range l.Backends {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("backend %q not in %+v", name, l.Backends)
	return MemoryBackendView{}
}

func TestMemoryEditing(t *testing.T) {
	m, dir := memoryFixture(t, memorySystem)

	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if !list.Editable || len(list.Profiles) != 0 || len(list.Backends) != 1 {
		t.Fatalf("initial list = %+v", list)
	}
	main := findBackend(t, list, "main_db")
	if !main.Implicit || main.Provider != "sqlite" {
		t.Fatalf("implicit main_db = %+v", main)
	}
	if len(list.BuiltinAgents) != 1 || list.BuiltinAgents[0] != "chat" {
		t.Errorf("agents on the built-in profile = %v", list.BuiltinAgents)
	}

	// The first backend in the file writes main_db out too, so the agent on
	// the built-in profile keeps its store.
	list, err = m.SaveBackend(MemoryBackendDraft{Name: "vectors", Provider: "sqlite", Path: "./data/vectors.db", VectorScanLimit: 5000}, true, list.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if main := findBackend(t, list, "main_db"); main.Implicit || main.Path != "./data/agentflow.db" {
		t.Fatalf("main_db after the first backend = %+v", main)
	}
	if v := findBackend(t, list, "vectors"); v.Path != "./data/vectors.db" || v.VectorScanLimit != 5000 || v.Live {
		t.Fatalf("new backend = %+v", v)
	}

	// Semantic recall without an embed model is refused before anything is
	// written; with one it saves.
	draft := MemoryProfileDraft{
		Name:   "rag",
		Recall: "plugin:semantic",
		Stores: []MemoryStoreDraft{
			{Name: "semantic", Backend: "vectors", Table: "passages", Requires: []string{"kv", "vector"}, Retention: "forever"},
			{Name: "dialogue", Backend: "main_db", Table: "rag_dialogue", Retention: "30d", Window: 200},
		},
	}
	if errs := m.ValidateProfile(draft, true); len(errs) == 0 || !strings.Contains(strings.Join(errs, ";"), "embed model") {
		t.Fatalf("semantic recall without an embed model: %v", errs)
	}
	draft.EmbedModel = "embed"
	if errs := m.ValidateProfile(draft, true); len(errs) != 0 {
		t.Fatalf("valid profile refused: %v", errs)
	}
	list, err = m.SaveProfile(draft, true, list.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Profiles) != 1 || list.Profiles[0].Name != "rag" || len(list.Profiles[0].Stores) != 2 {
		t.Fatalf("profiles after save = %+v", list.Profiles)
	}
	if v := findBackend(t, list, "vectors"); len(v.Profiles) != 1 || v.Profiles[0] != "rag" {
		t.Errorf("vectors usage = %+v", v)
	}

	// The saved directory still loads, and the profile reads back as written.
	cfg, err := config.LoadDir(dir, safetyLog())
	if err != nil {
		t.Fatalf("saved config does not load: %v", err)
	}
	got := cfg.Profiles.Memory["rag"]
	if got.EmbedModel != "embed" || got.Stores["dialogue"].Window != 200 || got.Stores["semantic"].Table != "passages" {
		t.Fatalf("persisted profile = %+v", got)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "system.yaml"))
	if strings.Contains(string(raw), "locality_ok") || strings.Contains(string(raw), "oversample") {
		t.Errorf("zero values were written:\n%s", raw)
	}

	// An unknown backend, a duplicate store and a bad retention are all
	// reported at once.
	bad := MemoryProfileDraft{Name: "bad", Stores: []MemoryStoreDraft{
		{Name: "a", Backend: "nowhere"}, {Name: "a", Backend: "main_db"}, {Name: "b", Backend: "main_db", Retention: "soon"},
	}}
	if errs := m.ValidateProfile(bad, true); len(errs) != 3 {
		t.Fatalf("errors for a bad profile = %v", errs)
	}
	if errs := m.ValidateProfile(MemoryProfileDraft{Name: "conversational", Stores: draft.Stores}, true); len(errs) == 0 {
		t.Fatal("the built-in profile name was accepted")
	}

	// Deletes are refused while something depends on the entry.
	if _, err := m.DeleteBackend("vectors", list.MTime); err == nil || !strings.Contains(err.Error(), "profile rag") {
		t.Fatalf("deleting a backend a profile uses: %v", err)
	}
	if _, err := m.DeleteBackend("main_db", list.MTime); err == nil || !strings.Contains(err.Error(), "chat") {
		t.Fatalf("deleting the built-in profile's backend: %v", err)
	}
	list, err = m.DeleteProfile("rag", list.MTime)
	if err != nil {
		t.Fatal(err)
	}
	list, err = m.DeleteBackend("vectors", list.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Profiles) != 0 || len(list.Backends) != 1 {
		t.Fatalf("after deletes = %+v", list)
	}

	// A stale mtime is a conflict, not a silent overwrite.
	if _, err := m.SaveBackend(MemoryBackendDraft{Name: "x", Provider: "volatile", MaxEntries: 10}, true, 1); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("stale mtime: %v", err)
	}
}

func TestMemoryProfileInUseCannotBeDeleted(t *testing.T) {
	m, dir := memoryFixture(t, memorySystem+`
  memory:
    notes:
      stores:
        facts: { backend: main_db, table: notes }
`)
	writeFile(t, filepath.Join(dir, "profiles", "chat.yaml"), "name: chat\nloop: ./chat.lua\nsafety: none\nmemory: notes\n")
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Profiles) != 1 || len(list.Profiles[0].Agents) != 1 {
		t.Fatalf("profile usage = %+v", list.Profiles)
	}
	if _, err := m.DeleteProfile("notes", list.MTime); err == nil || !strings.Contains(err.Error(), "chat") {
		t.Fatalf("deleting a profile an agent uses: %v", err)
	}
}

func TestMemoryBackendSecretsStayWriteOnly(t *testing.T) {
	m, dir := memoryFixture(t, memorySystem+`
memory:
  backends:
    main_db: { provider: sqlite, config: { path: ./data/agentflow.db } }
    pg: { provider: postgres, config: { url: "postgres://app:s3cret@db.internal:5432/mem", dim: 1536, password: kept } }
    qd: { provider: qdrant, config: { url: "${QDRANT_URL}", api_key: "cred:qdrant", collection: docs } }
`)
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	pg := findBackend(t, list, "pg")
	if !pg.HasURL || pg.URLRef != "" || strings.Contains(pg.URLDisplay, "s3cret") || !strings.Contains(pg.URLDisplay, "db.internal") {
		t.Fatalf("literal URL view = %+v", pg)
	}
	if len(pg.OtherKeys) != 1 || pg.OtherKeys[0] != "password" {
		t.Errorf("unknown keys = %v", pg.OtherKeys)
	}
	qd := findBackend(t, list, "qd")
	if qd.URLRef != "${QDRANT_URL}" || qd.APIKeyRef != "cred:qdrant" || !qd.HasAPIKey {
		t.Fatalf("reference view = %+v", qd)
	}

	// An empty URL keeps the stored one; keys the editor does not show stay.
	list, err = m.SaveBackend(MemoryBackendDraft{Name: "pg", Provider: "postgres", Dim: 768}, false, list.MTime)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadDir(dir, safetyLog())
	if err != nil {
		t.Fatal(err)
	}
	pgCfg := cfg.Memory.Backends["pg"].Config
	if pgCfg["url"] != "postgres://app:s3cret@db.internal:5432/mem" || pgCfg["dim"] != 768 || pgCfg["password"] != "kept" {
		t.Fatalf("pg config after an edit = %v", pgCfg)
	}

	// Changing provider drops the keys the new provider does not read.
	list, err = m.SaveBackend(MemoryBackendDraft{Name: "qd", Provider: "volatile", MaxEntries: 1000}, false, list.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if qd := findBackend(t, list, "qd"); qd.HasURL || qd.HasAPIKey || qd.Collection != "" || qd.MaxEntries != 1000 {
		t.Fatalf("after a provider change = %+v", qd)
	}

	// A provider that needs a URL refuses a backend without one.
	if errs := m.ValidateBackend(MemoryBackendDraft{Name: "r", Provider: "redis"}, true); len(errs) == 0 {
		t.Fatal("a redis backend without a URL was accepted")
	}
}

// An agent that names a memory profile round-trips through the builder's
// writer: before MemoryAgentConfig could marshal, it was written as a mapping
// the strict loader refused.
func TestAgentMemorySurvivesPersisting(t *testing.T) {
	for _, mem := range []string{"memory: conversational", "memory:\n  stores:\n    facts: { backend: main_db, table: notes }\n  recall: plugin:recency"} {
		var a config.Agent
		if err := yaml.Unmarshal([]byte("loop: ./a.lua\n"+mem+"\n"), &a); err != nil {
			t.Fatal(err)
		}
		n, err := agentNode(a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := yaml.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		var back config.Agent
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&back); err != nil {
			t.Fatalf("%q does not read back:\n%s\n%v", mem, b, err)
		}
		if back.Memory.Profile != a.Memory.Profile || back.Memory.IsInline != a.Memory.IsInline ||
			back.Memory.Inline.Stores["facts"].Table != a.Memory.Inline.Stores["facts"].Table {
			t.Fatalf("%q read back as %+v", mem, back.Memory)
		}
	}
}
