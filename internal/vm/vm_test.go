package vm

import (
	"strings"
	"testing"
)

func TestInstructionBudgetKillsInfiniteLoop(t *testing.T) {
	st := New(5_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@spin.lua", directive()+`
function loop()
  while true do end
end
`)
	if status != Failed || !strings.Contains(msg, "instruction budget exceeded") {
		t.Fatalf("infinite loop = (%v, %q); want Failed with instruction budget exceeded", status, msg)
	}
}

func TestInstructionBudgetCannotBeSwallowedByPCall(t *testing.T) {
	st := New(5_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@pcall-spin.lua", directive()+`
function loop()
  local ok, err = pcall(function()
    while true do end
  end)
  -- Must not keep running after budget exhaustion.
  local x = 0
  for i = 1, 1000 do x = x + i end
end
`)
	if status != Failed || !strings.Contains(msg, "instruction budget exceeded") {
		t.Fatalf("pcall spin = (%v, %q); want Failed with instruction budget exceeded", status, msg)
	}
}

func TestInstructionBudgetEnforcedInUserCoroutines(t *testing.T) {
	st := New(5_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@co-spin.lua", directive()+`
function loop()
  local co = coroutine.create(function()
    while true do end
  end)
  local ok, err = coroutine.resume(co)
  if not ok then error(err) end
end
`)
	if status != Failed || !strings.Contains(msg, "instruction budget exceeded") {
		t.Fatalf("coroutine spin = (%v, %q); want Failed with instruction budget exceeded", status, msg)
	}
}

func TestInstructionBudgetRearmsPerResume(t *testing.T) {
	st := New(20_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@rearm.lua", directive()+`
function loop()
  for round = 1, 5 do
    local s = 0
    for i = 1, 1000 do s = s + i end
    session.inbox()
  end
end
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

func TestYieldablePCallAndXPCallAcrossHostOps(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@yield-pcall.lua", directive()+`
function loop()
  -- Nested pcall across yielding op (ok = true)
  local ok1, ok2, res = pcall(function()
    return pcall(session.inbox)
  end)
  assert(ok1 == true, "outer pcall failed")
  assert(ok2 == true, "inner pcall failed")
  assert(res.text == "hello", "unexpected res: " .. tostring(res and res.text))

  -- pcall catches op failure (ok = false)
  local ok3, err3 = pcall(session.inbox)
  assert(ok3 == false, "expected pcall to catch op error")
  assert(string.find(tostring(err3), "denied", 1, true) ~= nil, "lost error: " .. tostring(err3))

  -- xpcall catches op failure and runs handler
  local ok4, err4 = xpcall(function() return session.inbox() end, function(e)
    return "handled:" .. tostring(e)
  end)
  assert(ok4 == false, "expected xpcall to catch op error")
  assert(string.find(tostring(err4), "handled:", 1, true) ~= nil, "xpcall handler did not run: " .. tostring(err4))
end
`)
	if status != Yielded {
		t.Fatalf("op 1: got (%v, %q); want Yielded", status, msg)
	}
	status, msg = st.Resume(`{"text":"hello"}`, true)
	if status != Yielded {
		t.Fatalf("op 2: got (%v, %q); want Yielded", status, msg)
	}
	status, msg = st.Resume(`permission denied`, false)
	if status != Yielded {
		t.Fatalf("op 3: got (%v, %q); want Yielded", status, msg)
	}
	status, msg = st.Resume(`boom`, false)
	if status != Finished {
		t.Fatalf("final: got (%v, %q); want Finished", status, msg)
	}
}

func TestSandboxStripsUnsafeGlobalsAndSealsStdlib(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@sandbox.lua", directive()+`
TOP_LEVEL = 42
function loop()
  -- Unsafe libraries/functions must not exist.
  for _, name in ipairs({
    "io", "debug", "package", "require", "dofile", "load",
    "loadfile", "loadstring", "module", "setfenv", "getfenv",
    "_printregs", "collectgarbage",
  }) do
    assert(_G[name] == nil, "unsafe global exposed: " .. name)
  end
  assert(os.execute == nil and os.remove == nil and os.exit == nil, "unsafe os.* exposed")
  assert(string.dump == nil, "string.dump exposed")

  -- Thread-local globals work, and string methods still work after sealing.
  assert(TOP_LEVEL == 42)
  assert(("hello"):sub(1, 3) == "hel")

  -- Sealed stdlib tables reject mutation via assignment, rawset, and table.insert.
  local ok1 = pcall(function() string.evil = 1 end)
  assert(not ok1, "mutating string table succeeded")
  local ok2 = pcall(function() rawset(string, "evil", 1) end)
  assert(not ok2, "rawset on sealed string table succeeded")
  local ok3 = pcall(function() table.insert(string, 1) end)
  assert(not ok3, "table.insert on sealed string table succeeded")
  local ok4 = pcall(function() setmetatable(_G, {}) end)
  assert(not ok4, "setmetatable(_G) succeeded")
end
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
	status, msg := st.Start("loop", "@phase0.lua", directive()+`
local boot = session.inbox()
function loop()
  assert(boot.ready == true)
  local m = session.inbox()
  assert(m.text == "after-boot")
end
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
	status, msg := st.Start("loop", "@noloop.lua", directive()+"local x = 1\n")
	if status != Failed || !strings.Contains(msg, "global 'loop' is not defined") {
		t.Fatalf("missing loop = (%v, %q); want Failed naming global 'loop'", status, msg)
	}
}

func TestMemoryCapGuardsStringRepAndTableConcat(t *testing.T) {
	st := New(1_000_000)
	st.SetMemoryCap(4096)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@memcap.lua", directive()+`
function loop()
  local ok1, err1 = pcall(string.rep, "abcdefgh", 1000)
  assert(not ok1 and string.find(tostring(err1), "out of memory", 1, true), "string.rep not capped: " .. tostring(err1))

  local parts = {}
  for i = 1, 200 do parts[i] = "012345678901234567890123456789" end
  local ok2, err2 = pcall(table.concat, parts)
  assert(not ok2 and string.find(tostring(err2), "out of memory", 1, true), "table.concat not capped: " .. tostring(err2))
end
`)
	if status != Finished {
		t.Fatalf("memcap test failed: (%v, %q)", status, msg)
	}
}

func TestUTF8Bit32AndCompatHelpers(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if err := st.Eval("@compat", `
-- utf8
local s = utf8.char(0x4E2D, 0x6587, 0x1F600)
assert(utf8.len(s) == 3, "utf8.len")
local a, b, c = utf8.codepoint(s, 1, #s)
assert(a == 0x4E2D and b == 0x6587 and c == 0x1F600, "utf8.codepoint")
local cps = {}
for _, cp in utf8.codes(s) do cps[#cps + 1] = cp end
assert(#cps == 3 and cps[3] == 0x1F600, "utf8.codes")
assert(utf8.offset(s, 2) == 4 and utf8.offset(s, -1) == 7, "utf8.offset")

-- bit32
assert(bit32.band(0xF0, 0x3C) == 0x30)
assert(bit32.bor(0xF0, 0x0F) == 0xFF)
assert(bit32.bxor(0xFF, 0x0F) == 0xF0)
assert(bit32.lshift(1, 8) == 256)
assert(bit32.rshift(256, 8) == 1)
assert(bit32.extract(0xABCD, 4, 8) == 0xBC)
assert(bit32.replace(0xABCD, 0x12, 4, 8) == 0xA12D)

-- table.* & string.split
local p = table.pack(10, nil, 30)
assert(p.n == 3)
local x, y, z = table.unpack({1, 2, 3})
assert(x == 1 and y == 2 and z == 3)
assert(table.find({"a", "b", "c"}, "b") == 2)
local cl = table.clone({k = 7})
assert(cl.k == 7)
table.clear(cl)
assert(next(cl) == nil)
local parts = string.split("a,b,c", ",")
assert(#parts == 3 and parts[2] == "b")
`); err != nil {
		t.Fatalf("compat helpers failed: %v", err)
	}
}
