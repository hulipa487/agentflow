package vm

import (
	"strings"
	"testing"
)

func TestCompileCacheServesChangedSource(t *testing.T) {
	st := New(0)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if err := st.Eval("@chunk", "var VERSION = 1;"); err != nil {
		t.Fatal(err)
	}
	if s, msg := st.Start("loop", "@loop.ts", directive()+`
function loop(): void {
  if (VERSION !== 1) throw new Error("stale");
}`); s == Failed {
		t.Fatalf("first start: %s", msg)
	}

	st2 := New(0)
	defer st2.Close()
	if err := st2.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if err := st2.Eval("@chunk", "var VERSION = 1;"); err != nil {
		t.Fatal(err)
	}
	if s, msg := st2.Start("loop", "@loop.ts", directive()+`
function loop(): void {
  if (VERSION !== 1) throw new Error("stale");
}`); s == Failed {
		t.Fatalf("second start: %s", msg)
	}

	st3 := New(0)
	defer st3.Close()
	if err := st3.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if err := st3.Eval("@chunk", "var VERSION = 2;"); err != nil {
		t.Fatal(err)
	}
	if s, msg := st3.Start("loop", "@loop.ts", directive()+`
function loop(): void {
  if (VERSION !== 2) throw new Error("stale");
}`); s == Failed {
		t.Fatalf("edited start: %s", msg)
	}
}

func TestCompileErrorCarriesTheChunkName(t *testing.T) {
	st := New(0)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	err := st.Eval("@broken.ts", "const =")
	if err == nil || !strings.Contains(err.Error(), "@broken.ts") {
		t.Fatalf("eval error = %v; want it to name @broken.ts", err)
	}
	status, msg := st.Start("loop", "@broken.ts", directive()+"function loop( {}")
	if status != Failed || !strings.Contains(msg, "@broken.ts") {
		t.Fatalf("start = %v %q; want a named compile error", status, msg)
	}
}
