package builtins

import (
	"os"
	"path/filepath"
	"testing"

	"agentflow/internal/vm"
)

// TestExampleLoopsCompile keeps the documented examples honest: every loop
// under examples/loops and every scenario's loops (examples/<scenario>/loops)
// must declare this core's prelude version and compile. A docs snippet can rot
// silently; a checked-in loop cannot.
func TestExampleLoopsCompile(t *testing.T) {
	var paths []string
	for _, pattern := range []string{
		filepath.Join("..", "..", "examples", "loops", "*.lua"),
		filepath.Join("..", "..", "examples", "*", "loops", "*.lua"),
	} {
		found, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, found...)
	}
	if len(paths) == 0 {
		t.Fatal("no example loops found")
	}
	for _, path := range paths {
		name, _ := filepath.Rel(filepath.Join("..", "..", "examples"), path)
		t.Run(filepath.ToSlash(name), func(t *testing.T) {
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
