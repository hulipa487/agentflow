package builtins

import (
	"os"
	"path/filepath"
	"testing"

	"agentflow/internal/vm"
)

// TestExampleLoopsCompile keeps the documented examples honest: every loop
// under examples/loops must declare this core's prelude version and compile.
// A docs snippet can rot silently; a checked-in loop cannot.
func TestExampleLoopsCompile(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "examples", "loops", "*.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no example loops found")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := vm.CheckChunkVersion(path, string(src)); err != nil {
				t.Fatalf("prelude version: %v", err)
			}
			if err := vm.CompileCheck(path, string(src)); err != nil {
				t.Fatalf("compile: %v", err)
			}
		})
	}
}
