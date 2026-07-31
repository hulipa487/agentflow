package vm

import (
	"os"
	"path/filepath"
	"testing"
)

// The loops that now drive native tool-calling must parse. A syntax slip in
// the Lua would only surface at session boot; compile-check them here so the
// gate catches it.
func TestToolCallingLoopsParse(t *testing.T) {
	files := []string{
		filepath.Join("..", "builtins", "lua", "react.lua"),
		filepath.Join("..", "..", "plugins", "orchestrator", "main.lua"),
		filepath.Join("..", "..", "plugins", "orchestrator", "expert.lua"),
		filepath.Join("..", "..", "plugins", "orchestrator", "pm.lua"),
		filepath.Join("..", "..", "plugins", "orchestrator", "worker.lua"),
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
