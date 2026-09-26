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

// directive renders the prelude version line a fixture carries, so a test that
// only needs *a* well-formed chunk does not have to be edited when the version
// moves. The chunks this repo actually ships spell the number literally, and
// TestPreludeContract is what keeps those literals honest.
func directive() string { return fmt.Sprintf("-- af-prelude-version: %d\n", vm.PreludeVersion) }

// TestPreludeContract runs one contract against every Lua chunk in the tree,
// the way TestBackendContract runs one contract against every memory backend.
//
// The contract is the version gate plus a compile: a chunk must declare the
// prelude API version it targets, that version must be the one this core
// provides, and this core's Luau must still parse it. The first two are the
// separation's commercial safety net — core and proprietary Lua move on
// separate release cadences, so something has to fail on our side when the API
// moves rather than on a customer's first turn.
//
// The proprietary Lua repo runs this suite in its CI, from a checkout of this
// repo at the core version it is pinned to:
//
//	AF_CONFORMANCE_LUA_DIR=/path/to/proprietary/lua go test ./internal/builtins/ -run TestPreludeContract
//
// Every *.lua under that directory is a case. A core release that changes the
// prelude's surface bumps vm.PreludeVersion; a chunk left declaring the old
// version fails here, in the other repo's build, which is the whole point.
//
// The env var is the shape rather than an exported Go API because everything in
// this module is under internal/, which a separate module cannot import — so
// there is nothing to import, and the suite has to travel as source the way the
// backends' contract does. With the variable unset the suite still checks this
// repo's own six embedded chunks, which is what makes it a build check here and
// a compatibility check there.
//
// What it does NOT cover: whether the chunk uses an op that no longer exists.
// That needs the chunk to run, and running a loop is not something a test can
// do to arbitrary sources. The version declaration is the contract that stands
// in for it, so a core release that removes an op without bumping the version
// is caught by review of the prelude diff, not here.
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

	dir := os.Getenv("AF_CONFORMANCE_LUA_DIR")
	if dir == "" {
		t.Log("AF_CONFORMANCE_LUA_DIR is unset: this repo's own chunks were checked, no external tree was")
		return
	}
	root := filepath.Clean(dir)
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".lua") {
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
		t.Fatalf("walking AF_CONFORMANCE_LUA_DIR=%s: %v", dir, err)
	}
	// A directory with nothing in it would make the suite pass while checking
	// nothing, which is worse than failing: the caller believes the Lua was
	// validated.
	if checked == 0 {
		t.Fatalf("AF_CONFORMANCE_LUA_DIR=%s holds no *.lua files; the suite checked nothing", dir)
	}
	t.Logf("checked %d chunk(s) under %s", checked, dir)
}

// runPreludeContract is the contract one chunk owes this core. Both assertions
// are refusals, not warnings: there is no compatibility window, so a chunk that
// cannot be served is a chunk the core must not load.
func runPreludeContract(t *testing.T, name, src string) {
	t.Helper()

	// CheckChunkVersion is the gate the runtime itself uses, so a passing
	// contract here is the same answer the boot path and the hot reload path
	// give. It covers both halves: that the chunk declares a version at all
	// (silence is refused, never assumed compatible) and that the version is
	// this core's.
	if err := vm.CheckChunkVersion(name, src); err != nil {
		t.Fatal(err)
	}

	// The version says which API the chunk was written for; the compile says
	// the core can still read the syntax. A Luau-level change that broke a
	// chunk's syntax would otherwise slip past a version that nobody bumped.
	if err := vm.CompileCheck(name, src); err != nil {
		t.Fatalf("%s does not compile on this core's Luau: %v", name, err)
	}
}
