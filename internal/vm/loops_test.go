package vm

import (
	"os"
	"path/filepath"
	"testing"
)

// Builtin loops shipped with the runtime must parse. A syntax slip would only
// surface at session boot; compile-check them here so the gate catches it.
// Product loops (e.g. an orchestrator) live outside this repo and are checked
// there.
func TestShippedLoopsParse(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "builtins", "lua", "*.lua"))
	if err != nil {
		t.Fatalf("glob builtins/lua: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no loop files found to compile-check")
	}
	for _, f := range files {
		code, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if err := CompileCheck(f, string(code)); err != nil {
			t.Errorf("%s does not parse: %v", f, err)
		}
	}
}
