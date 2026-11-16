package vm

import (
	"strings"
	"testing"
)

// TestCompileCacheServesChangedSource: the cache is content-addressed, so the
// same source hits it and an edited source compiles anew. A stale bytecode
// entry would be a hot-reload serving the old loop.
func TestCompileCacheServesChangedSource(t *testing.T) {
	st := New(0)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if err := st.Eval("@chunk", "VERSION = 1"); err != nil {
		t.Fatal(err)
	}
	if s, msg := st.Start("loop", "@loop", `-- af-prelude-version: 1
function loop()
  if VERSION ~= 1 then error("stale") end
end`); s == Failed {
		t.Fatalf("first start: %s", msg)
	}

	// The same source again: a cache hit.
	st2 := New(0)
	defer st2.Close()
	if err := st2.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if err := st2.Eval("@chunk", "VERSION = 1"); err != nil {
		t.Fatal(err)
	}
	if s, msg := st2.Start("loop", "@loop", `-- af-prelude-version: 1
function loop()
  if VERSION ~= 1 then error("stale") end
end`); s == Failed {
		t.Fatalf("second start: %s", msg)
	}

	// An edited source has a different hash: it must compile the new body, not
	// load the cached old one.
	st3 := New(0)
	defer st3.Close()
	if err := st3.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if err := st3.Eval("@chunk", "VERSION = 2"); err != nil {
		t.Fatal(err)
	}
	if s, msg := st3.Start("loop", "@loop", `-- af-prelude-version: 1
function loop()
  if VERSION ~= 2 then error("stale") end
end`); s == Failed {
		t.Fatalf("edited start: %s", msg)
	}
}

// TestCompileErrorCarriesTheChunkName: a refusal must name the file to edit,
// exactly as the load path did when it compiled inline.
func TestCompileErrorCarriesTheChunkName(t *testing.T) {
	st := New(0)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	err := st.Eval("@broken.lua", "local =")
	if err == nil || !strings.Contains(err.Error(), "@broken.lua:") {
		t.Fatalf("eval error = %v; want it to name @broken.lua", err)
	}
	status, msg := st.Start("loop", "@broken.lua", "-- af-prelude-version: 1\nfunction loop( end")
	if status != Failed || !strings.Contains(msg, "@broken.lua:") {
		t.Fatalf("start = %v %q; want a named compile error", status, msg)
	}
}
