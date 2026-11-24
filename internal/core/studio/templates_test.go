package studio

import (
	"slices"
	"testing"

	"agentflow/internal/vm"
)

// Every starter template must load as written: the current prelude version and
// a chunk that compiles, so an agent created from one runs without an edit.
func TestTemplatesCompile(t *testing.T) {
	tpls := Templates()
	wantOrder := []string{
		"qq-group-bot-fts",
		"qq-group-bot-vec",
		"telegram-group-bot-fts",
		"telegram-group-bot-vec",
	}
	if len(tpls) != len(wantOrder) {
		t.Fatalf("expected exactly %d templates, got %d", len(wantOrder), len(tpls))
	}

	seen := map[string]bool{}
	for i, tpl := range tpls {
		if tpl.Name == "" || tpl.Title == "" || tpl.Description == "" {
			t.Fatalf("template %+v is missing a name, title or description", tpl)
		}
		if tpl.Name != wantOrder[i] {
			t.Fatalf("template[%d].Name = %q, want %q", i, tpl.Name, wantOrder[i])
		}
		if seen[tpl.Name] {
			t.Fatalf("template name %q is used twice", tpl.Name)
		}
		seen[tpl.Name] = true
		t.Run(tpl.Name, func(t *testing.T) {
			path := tpl.Name + ".ts"
			if err := vm.CheckChunkVersion(path, tpl.Loop); err != nil {
				t.Fatalf("prelude version: %v", err)
			}
			if err := vm.CompileCheck(path, tpl.Loop); err != nil {
				t.Fatalf("compile: %v", err)
			}
			if len(tpl.Settings) == 0 || len(tpl.Extras) == 0 || tpl.Instructions == "" {
				t.Fatalf("missing settings/extras/instructions on %s", tpl.Name)
			}
			if len(tpl.Capabilities) == 0 || len(tpl.Skills) == 0 {
				t.Fatalf("missing capabilities/skills on %s", tpl.Name)
			}
			if len(tpl.MemoryInline) == 0 {
				t.Fatalf("missing inline memory profile on %s", tpl.Name)
			}
			stores, ok := tpl.MemoryInline["stores"].(map[string]any)
			if !ok {
				t.Fatalf("memory_inline.stores missing on %s: %#v", tpl.Name, tpl.MemoryInline)
			}
			dialogue, ok := stores["dialogue"].(map[string]any)
			if !ok {
				t.Fatalf("memory_inline.stores.dialogue missing on %s", tpl.Name)
			}
			requires, _ := dialogue["requires"].([]string)
			switch tpl.Name {
			case "qq-group-bot-fts", "telegram-group-bot-fts":
				if tpl.MemoryInline["recall"] != "plugin:recency" {
					t.Fatalf("expected plugin:recency on %s, got %v", tpl.Name, tpl.MemoryInline["recall"])
				}
				if !slices.Contains(requires, "text_search") {
					t.Fatalf("expected text_search requirement on %s, got %v", tpl.Name, requires)
				}
			case "qq-group-bot-vec", "telegram-group-bot-vec":
				if tpl.MemoryInline["recall"] != "plugin:semantic" || tpl.MemoryInline["embed_model"] != "bge-m3" {
					t.Fatalf("expected plugin:semantic + bge-m3 on %s, got %#v", tpl.Name, tpl.MemoryInline)
				}
				if !slices.Contains(requires, "vector") {
					t.Fatalf("expected vector requirement on %s, got %v", tpl.Name, requires)
				}
			}
		})
	}
}
