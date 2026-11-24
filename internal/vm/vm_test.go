package vm

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInstructionBudgetKillsInfiniteLoop(t *testing.T) {
	st := New(5_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@spin.ts", directive()+`
function loop(): void {
  while (true) {}
}
`)
	if status != Failed || !strings.Contains(msg, "instruction budget exceeded") {
		t.Fatalf("infinite loop = (%v, %q); want Failed with instruction budget exceeded", status, msg)
	}
}

func TestInstructionBudgetCannotBeSwallowedByTryCatch(t *testing.T) {
	st := New(5_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@try-spin.ts", directive()+`
function loop(): void {
  try {
    while (true) {}
  } catch (e) {}
  let x = 0;
  for (let i = 0; i < 1000; i++) x += i;
}
`)
	if status != Failed || !strings.Contains(msg, "instruction budget exceeded") {
		t.Fatalf("try/catch spin = (%v, %q); want Failed with instruction budget exceeded", status, msg)
	}
}

func TestInstructionBudgetRearmsPerResume(t *testing.T) {
	st := New(20_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@rearm.ts", directive()+`
function loop(): void {
  for (let round = 0; round < 5; round++) {
    let s = 0;
    for (let i = 0; i < 1000; i++) s += i;
    session.inbox();
  }
}
`)
	for round := 0; round < 5; round++ {
		if status != Yielded {
			t.Fatalf("round %d: got (%v, %q); want Yielded", round, status, msg)
		}
		status, msg = st.Resume(`{"text":"hi"}`, true)
	}
	if status != Finished {
		t.Fatalf("final status = (%v, %q); want Finished", status, msg)
	}
}

func TestTryCatchAcrossHostOps(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@yield-try.ts", directive()+`
function loop(): void {
  let inner = null;
  try {
    inner = session.inbox();
  } catch (e) {
    throw new Error("unexpected error: " + String(e));
  }
  if (inner.text !== "hello") throw new Error("unexpected text: " + inner.text);

  let caught = "";
  try {
    session.inbox();
  } catch (e) {
    caught = String(e);
  }
  if (!caught.includes("denied")) throw new Error("lost error: " + caught);
}
`)
	if status != Yielded {
		t.Fatalf("op 1: got (%v, %q); want Yielded", status, msg)
	}
	status, msg = st.Resume(`{"text":"hello"}`, true)
	if status != Yielded {
		t.Fatalf("op 2: got (%v, %q); want Yielded", status, msg)
	}
	status, msg = st.Resume(`"permission denied"`, false)
	if status != Finished {
		t.Fatalf("final: got (%v, %q); want Finished", status, msg)
	}
}

func TestSandboxStripsUnsafeGlobalsAndSealsNamespaces(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@sandbox.ts", directive()+`
var TOP_LEVEL = 42;
export function loop(): void {
  const assert = (c: boolean, m?: string) => { if (!c) throw new Error(m || "assertion failed"); };
  for (const name of ["io", "debug", "package", "require", "process", "fs", "fetch", "Buffer"]) {
    assert(typeof (globalThis as Record<string, unknown>)[name] === "undefined", "unsafe global exposed: " + name);
  }
  assert(os.execute === undefined && os.remove === undefined && os.exit === undefined, "unsafe os.* exposed");
  assert(TOP_LEVEL === 42);

  // Sealed prelude namespaces reject property writes/reassignment.
  assert(Reflect.set(os, "evil", 1) === false, "mutating sealed os succeeded");
  assert(Reflect.set(http, "request", () => {}) === false, "mutating sealed http succeeded");

  let proxyThrew = false;
  try {
    (api as Record<string, unknown>).evil = 1;
  } catch (_) {
    proxyThrew = true;
  }
  assert(proxyThrew, "api proxy allowed property write");
}
`)
	if status != Finished {
		t.Fatalf("sandbox test failed: (%v, %q)", status, msg)
	}
}

func TestPhase0YieldChainsIntoLoop(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@phase0.ts", directive()+`
const boot = session.inbox();
function loop(): void {
  if (boot.ready !== true) throw new Error("boot missing");
  const m = session.inbox();
  if (m.text !== "after-boot") throw new Error("wrong text");
}
`)
	if status != Yielded {
		t.Fatalf("phase 0 yield: got (%v, %q); want Yielded", status, msg)
	}
	status, msg = st.Resume(`{"ready":true}`, true)
	if status != Yielded {
		t.Fatalf("phase 1 yield: got (%v, %q); want Yielded", status, msg)
	}
	status, msg = st.Resume(`{"text":"after-boot"}`, true)
	if status != Finished {
		t.Fatalf("phase 1 finish: got (%v, %q); want Finished", status, msg)
	}
}

func TestMissingLoopGlobalFailsClearly(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@noloop.ts", directive()+"const x = 1;\n")
	if status != Failed || !strings.Contains(msg, "global 'loop' is not defined") {
		t.Fatalf("missing loop = (%v, %q); want Failed naming global 'loop'", status, msg)
	}
}

func TestMemoryCapGuardsRepeatAndArrayJoin(t *testing.T) {
	st := New(1_000_000)
	st.SetMemoryCap(4096)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@memcap.ts", directive()+`
function loop(): void {
  let err1 = "";
  try {
    "abcdefgh".repeat(1000);
  } catch (e) {
    err1 = String(e);
  }
  if (!err1.includes("out of memory")) throw new Error("repeat not capped: " + err1);

  const parts: string[] = [];
  for (let i = 0; i < 200; i++) parts.push("012345678901234567890123456789");
  let err2 = "";
  try {
    parts.join("");
  } catch (e) {
    err2 = String(e);
  }
  if (!err2.includes("out of memory")) throw new Error("join not capped: " + err2);
}
`)
	if status != Finished {
		t.Fatalf("memcap test failed: (%v, %q)", status, msg)
	}
}

func TestCloseMidYieldHasZeroGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	for i := 0; i < 25; i++ {
		st := New(1_000_000)
		if err := st.LoadBase(); err != nil {
			t.Fatal(err)
		}
		status, msg := st.Start("loop", "@leak.ts", directive()+`
function loop(): void {
  try {
    while (true) {
      session.inbox();
    }
  } catch (_) {}
}
`)
		if status != Yielded {
			st.Close()
			t.Fatalf("iter %d: expected Yielded, got (%v, %q)", i, status, msg)
		}
		st.Close()
	}
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > base+2 {
		t.Fatalf("goroutines leaked: before=%d after=%d", base, after)
	}
}
