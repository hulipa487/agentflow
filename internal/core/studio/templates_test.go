package studio

import (
	"testing"

	"agentflow/internal/vm"
)

// Every starter template must load as written: the current prelude version and
// a chunk that compiles, so an agent created from one runs without an edit.
func TestTemplatesCompile(t *testing.T) {
	seen := map[string]bool{}
	for _, tpl := range Templates() {
		if tpl.Name == "" || tpl.Title == "" || tpl.Description == "" {
			t.Fatalf("template %+v is missing a name, title or description", tpl)
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
		})
	}
	for _, want := range []string{"telegram-group-bot", "echo", "llm-chat", "group-chat", "rag", "single-turn-api"} {
		if !seen[want] {
			t.Fatalf("template %q is missing", want)
		}
	}
	var tg *Template
	for _, tpl := range Templates() {
		if tpl.Name == "telegram-group-bot" {
			copyTpl := tpl
			tg = &copyTpl
			break
		}
	}
	if tg == nil || len(tg.Settings) == 0 || len(tg.Extras) == 0 || tg.Memory != "conversational" || tg.Instructions == "" {
		t.Fatalf("telegram-group-bot preset missing schema metadata: %+v", tg)
	}
	for _, c := range tg.Capabilities {
		if c == "tools" || c == "apis" || c == "net.http" {
			t.Fatalf("telegram-group-bot must keep external write capability %q off by default", c)
		}
	}
}
