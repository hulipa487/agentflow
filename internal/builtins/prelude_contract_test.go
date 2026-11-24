package builtins

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"agentflow/internal/vm"
)

func directive() string { return fmt.Sprintf("// af-prelude-version: %d\n", vm.PreludeVersion) }

func isContractScript(name string) bool {
	if strings.HasSuffix(name, ".d.ts") {
		return false
	}
	return strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".js")
}

// TestPreludeContract validates every embedded builtin script chunk and any
// external conformance tree (AF_CONFORMANCE_SCRIPT_DIR or AF_CONFORMANCE_LUA_DIR)
// against the current prelude version gate and TypeScript/JS Goja compiler.
func TestPreludeContract(t *testing.T) {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		name := name
		t.Run("builtin:"+name, func(t *testing.T) {
			runPreludeContract(t, "builtin:"+name, sources[name])
		})
	}

	dir := os.Getenv("AF_CONFORMANCE_SCRIPT_DIR")
	if dir == "" {
		dir = os.Getenv("AF_CONFORMANCE_LUA_DIR")
	}
	if dir == "" {
		t.Log("AF_CONFORMANCE_SCRIPT_DIR is unset: this repo's own chunks were checked, no external tree was")
		return
	}
	root := filepath.Clean(dir)
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !isContractScript(d.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		t.Run("external:"+filepath.ToSlash(rel), func(t *testing.T) {
			runPreludeContract(t, path, string(b))
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking conformance script dir %s: %v", dir, err)
	}
	if checked == 0 {
		t.Fatalf("conformance script dir %s holds no *.ts/*.js files; the suite checked nothing", dir)
	}
	t.Logf("checked %d chunk(s) under %s", checked, dir)
}

func runPreludeContract(t *testing.T, name, src string) {
	t.Helper()
	if err := vm.CheckChunkVersion(name, src); err != nil {
		t.Fatal(err)
	}
	if err := vm.CompileCheck(name, src); err != nil {
		t.Fatalf("%s does not compile on this core's Goja runtime: %v", name, err)
	}
}
