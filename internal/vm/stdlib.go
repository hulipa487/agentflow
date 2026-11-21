package vm

import (
	"math/bits"
	"strings"
	"time"
	"unicode/utf8"

	lua "github.com/yuin/gopher-lua"
)

// unsafeBaseGlobals are stripped immediately after lua.OpenBase so user loops
// cannot escape the sandbox, load arbitrary files/code, or manipulate function
// environments.
var unsafeBaseGlobals = []string{
	"dofile",
	"load",
	"loadfile",
	"loadstring",
	"module",
	"require",
	"setfenv",
	"getfenv",
	"_printregs",
	"collectgarbage",
}

// openLibs installs the selective standard library and host bridge functions on
// s.L: base (stripped), coroutine, table, string, math, utf8, and bit32. No os,
// io, debug, package, or channel library is opened.
func (s *State) openLibs() {
	L := s.L

	lua.OpenBase(L)
	L.Pop(1)
	for _, name := range unsafeBaseGlobals {
		L.SetGlobal(name, lua.LNil)
	}

	// Guard rawset so user code cannot mutate sealed tables by bypassing __newindex.
	L.SetGlobal("rawset", L.NewFunction(func(L *lua.LState) int {
		tbl := L.CheckTable(1)
		key := L.CheckAny(2)
		val := L.CheckAny(3)
		if s.sealed[tbl] {
			L.RaiseError("attempt to modify a readonly table")
			return 0
		}
		if key == lua.LNil {
			L.RaiseError("table index is nil")
			return 0
		}
		tbl.RawSet(key, val)
		L.Push(tbl)
		return 1
	}))

	lua.OpenCoroutine(L)
	L.Pop(1)
	s.wrapCoroutineCreate()

	lua.OpenTable(L)
	L.Pop(1)
	s.extendTableLib()

	lua.OpenString(L)
	L.Pop(1)
	s.extendStringLib()

	lua.OpenMath(L)
	L.Pop(1)

	openUTF8(L)
	openBit32(L)

	// Host bridge: __af_op(request_json) -> yields request_json; Go resumes
	// with (response_json, ok).
	L.SetGlobal("__af_op", L.NewFunction(func(L *lua.LState) int {
		req := L.CheckString(1)
		// Refuse yields across a Go callback boundary (e.g. string.gsub or
		// table.sort) where gopher-lua's recursive callR cannot suspend.
		for level := 1; ; level++ {
			dbg, ok := L.GetStack(level)
			if !ok {
				break
			}
			if _, err := L.GetInfo("S", dbg, lua.LNil); err == nil && dbg.What == "G" {
				L.RaiseError("attempt to yield across a C-call boundary")
				return 0
			}
		}
		return L.Yield(lua.LString(req))
	}))

	// Wall clock: __af_now() -> Unix seconds. Non-yielding so os.time/os.date
	// from the prelude work in Eval as well as inside the loop coroutine.
	L.SetGlobal("__af_now", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(time.Now().Unix()))
		return 1
	}))

	// Replace Lua 5.1's non-yieldable pcall/xpcall with coroutine-backed
	// yieldable equivalents so loops can wrap yielding host ops like
	// pcall(llm.chat, ...) or pcall(tools.run, ...) just as in Luau / Lua 5.2+.
	installYieldablePCall(L)
}

// installYieldablePCall replaces global pcall and xpcall with pure-Lua
// coroutine wrappers that forward yields transparently up to the host loop
// driver while still catching errors and preserving multiple return values.
func installYieldablePCall(L *lua.LState) {
	const src = `
local __raw_pcall = pcall
local __co_create = coroutine.create
local __co_resume = coroutine.resume
local __co_status = coroutine.status
local __co_yield  = coroutine.yield
local __type      = type
local __getmt     = getmetatable
local __error     = error

local function __wrap_callable(fn)
  if __type(fn) == "function" then
    return fn, true
  end
  local mt = __getmt(fn)
  if __type(mt) == "table" and __type(mt.__call) == "function" then
    return function(...) return fn(...) end, true
  end
  return nil, false
end

function pcall(fn, ...)
  local target, callable = __wrap_callable(fn)
  if not callable then
    return false, "attempt to call a " .. __type(fn) .. " value"
  end
  local co = __co_create(target)
  local function step(ok, ...)
    if not ok then
      return false, ...
    end
    if __co_status(co) == "dead" then
      return true, ...
    end
    return step(__co_resume(co, __co_yield(...)))
  end
  return step(__co_resume(co, ...))
end

function xpcall(fn, errh, ...)
  if __type(errh) ~= "function" then
    __error("bad argument #2 to 'xpcall' (function expected)", 2)
  end
  local target, callable = __wrap_callable(fn)
  if not callable then
    local hok, herr = __raw_pcall(errh, "attempt to call a " .. __type(fn) .. " value")
    if not hok then return false, "error in error handling" end
    return false, herr
  end
  local co = __co_create(target)
  local function step(ok, ...)
    if not ok then
      local e = ...
      local hok, herr = __raw_pcall(errh, e)
      if not hok then return false, "error in error handling" end
      return false, herr
    end
    if __co_status(co) == "dead" then
      return true, ...
    end
    return step(__co_resume(co, __co_yield(...)))
  end
  return step(__co_resume(co, ...))
end
`
	if err := L.DoString(src); err != nil {
		panic("vm: installYieldablePCall: " + err.Error())
	}
}

// wrapCoroutineCreate ensures coroutines spawned by user code via
// coroutine.create or coroutine.wrap share the state's instruction budget
// context instead of inheriting an untracked context.
func (s *State) wrapCoroutineCreate() {
	L := s.L
	coTbl, ok := L.GetGlobal(lua.CoroutineLibName).(*lua.LTable)
	if !ok {
		return
	}
	origCreate := coTbl.RawGetString("create")
	wrappedCreate := L.NewFunction(func(L *lua.LState) int {
		fn := L.CheckFunction(1)
		L.Push(origCreate)
		L.Push(fn)
		L.Call(1, 1)
		if th, ok := L.Get(-1).(*lua.LState); ok {
			if s.budget != nil {
				th.SetContext(s.budget)
			}
			th.Env = L.Env
		}
		return 1
	})
	coTbl.RawSetString("create", wrappedCreate)

	// Re-implement coroutine.wrap on top of our wrapped create so wrapped
	// coroutines also inherit the instruction budget and thread environment.
	origResume := coTbl.RawGetString("resume")
	coTbl.RawSetString("wrap", L.NewFunction(func(L *lua.LState) int {
		fn := L.CheckFunction(1)
		L.Push(wrappedCreate)
		L.Push(fn)
		L.Call(1, 1)
		th := L.CheckThread(-1)
		L.Pop(1)
		L.Push(L.NewClosure(func(L *lua.LState) int {
			nargs := L.GetTop()
			L.Push(origResume)
			L.Push(th)
			for i := 1; i <= nargs; i++ {
				L.Push(L.Get(i))
			}
			L.Call(nargs+1, lua.MultRet)
			nret := L.GetTop() - nargs
			if nret < 1 {
				return 0
			}
			okVal := L.Get(nargs + 1)
			if !lua.LVAsBool(okVal) {
				errObj := L.Get(nargs + 2)
				L.Error(errObj, 1)
				return 0
			}
			// Shift return values down (dropping the leading true).
			for i := 2; i <= nret; i++ {
				L.Replace(i-1, L.Get(nargs+i))
			}
			L.SetTop(nret - 1)
			return nret - 1
		}))
		return 1
	}))
}

func (s *State) extendTableLib() {
	L := s.L
	tbl, ok := L.GetGlobal(lua.TabLibName).(*lua.LTable)
	if !ok {
		return
	}

	// Lua 5.2+ / Luau compatibility helpers.
	if unpackFn := L.GetGlobal("unpack"); unpackFn != lua.LNil {
		tbl.RawSetString("unpack", unpackFn)
	}
	tbl.RawSetString("pack", L.NewFunction(func(L *lua.LState) int {
		n := L.GetTop()
		out := L.CreateTable(n, 1)
		for i := 1; i <= n; i++ {
			out.RawSetInt(i, L.Get(i))
		}
		out.RawSetString("n", lua.LNumber(n))
		L.Push(out)
		return 1
	}))
	tbl.RawSetString("clear", L.NewFunction(func(L *lua.LState) int {
		t := L.CheckTable(1)
		if s.sealed[t] {
			L.RaiseError("attempt to modify a readonly table")
			return 0
		}
		var keys []lua.LValue
		t.ForEach(func(k, _ lua.LValue) {
			keys = append(keys, k)
		})
		for _, k := range keys {
			t.RawSet(k, lua.LNil)
		}
		return 0
	}))
	tbl.RawSetString("clone", L.NewFunction(func(L *lua.LState) int {
		src := L.CheckTable(1)
		dst := L.NewTable()
		src.ForEach(func(k, v lua.LValue) {
			dst.RawSet(k, v)
		})
		L.Push(dst)
		return 1
	}))
	tbl.RawSetString("find", L.NewFunction(func(L *lua.LState) int {
		t := L.CheckTable(1)
		needle := L.CheckAny(2)
		init := L.OptInt(3, 1)
		if init < 1 {
			init = 1
		}
		n := t.Len()
		for i := init; i <= n; i++ {
			if t.RawGetInt(i) == needle {
				L.Push(lua.LNumber(i))
				return 1
			}
		}
		L.Push(lua.LNil)
		return 1
	}))
	tbl.RawSetString("move", L.NewFunction(func(L *lua.LState) int {
		a1 := L.CheckTable(1)
		f := L.CheckInt(2)
		e := L.CheckInt(3)
		t := L.CheckInt(4)
		a2 := a1
		if L.GetTop() >= 5 && L.Get(5) != lua.LNil {
			a2 = L.CheckTable(5)
		}
		if s.sealed[a2] {
			L.RaiseError("attempt to modify a readonly table")
			return 0
		}
		if e >= f {
			n := e - f + 1
			if t > f && t <= e && a1 == a2 {
				for i := n - 1; i >= 0; i-- {
					a2.RawSetInt(t+i, a1.RawGetInt(f+i))
				}
			} else {
				for i := 0; i < n; i++ {
					a2.RawSetInt(t+i, a1.RawGetInt(f+i))
				}
			}
		}
		L.Push(a2)
		return 1
	}))

	// Guard mutating table stdlib calls against sealed tables.
	for _, fnName := range []string{"insert", "remove", "sort"} {
		orig := tbl.RawGetString(fnName)
		if orig == lua.LNil {
			continue
		}
		tbl.RawSetString(fnName, L.NewFunction(func(L *lua.LState) int {
			t := L.CheckTable(1)
			if s.sealed[t] {
				L.RaiseError("attempt to modify a readonly table")
				return 0
			}
			nargs := L.GetTop()
			L.Push(orig)
			for idx := 1; idx <= nargs; idx++ {
				L.Push(L.Get(idx))
			}
			L.Call(nargs, lua.MultRet)
			return L.GetTop() - nargs
		}))
	}

	// Guard table.concat against exceeding memCap.
	origConcat := tbl.RawGetString("concat")
	tbl.RawSetString("concat", L.NewFunction(func(L *lua.LState) int {
		t := L.CheckTable(1)
		sep := L.OptString(2, "")
		i := L.OptInt(3, 1)
		j := L.OptInt(4, t.Len())
		if s.memCap > 0 && j >= i {
			if i < 1 {
				i = 1
			}
			if n := t.Len(); j > n {
				j = n
			}
			var total int64
			sepLen := int64(len(sep))
			for idx := i; idx <= j; idx++ {
				v := t.RawGetInt(idx)
				switch sv := v.(type) {
				case lua.LString:
					total += int64(len(sv))
				case lua.LNumber:
					total += int64(len(sv.String()))
				}
				if idx < j {
					total += sepLen
				}
				if total > s.memCap {
					L.RaiseError("out of memory")
					return 0
				}
			}
		}
		nargs := L.GetTop()
		L.Push(origConcat)
		for idx := 1; idx <= nargs; idx++ {
			L.Push(L.Get(idx))
		}
		L.Call(nargs, 1)
		return 1
	}))
}

func (s *State) extendStringLib() {
	L := s.L
	strTbl, ok := L.GetGlobal(lua.StringLibName).(*lua.LTable)
	if !ok {
		return
	}
	strTbl.RawSetString("dump", lua.LNil)

	// Guard string.rep against exceeding memCap.
	strTbl.RawSetString("rep", L.NewFunction(func(L *lua.LState) int {
		str := L.CheckString(1)
		n := L.CheckInt(2)
		if n <= 0 {
			L.Push(lua.LString(""))
			return 1
		}
		if s.memCap > 0 && len(str) > 0 && int64(n) > s.memCap/int64(len(str)) {
			L.RaiseError("out of memory")
			return 0
		}
		L.Push(lua.LString(strings.Repeat(str, n)))
		return 1
	}))

	// Luau string.split(s, sep?) helper (default separator is ",").
	strTbl.RawSetString("split", L.NewFunction(func(L *lua.LState) int {
		str := L.CheckString(1)
		sep := L.OptString(2, ",")
		parts := strings.Split(str, sep)
		out := L.CreateTable(len(parts), 0)
		for i, p := range parts {
			out.RawSetInt(i+1, lua.LString(p))
		}
		L.Push(out)
		return 1
	}))
}

// ------------------------------------------------------------------ utf8 ---

func openUTF8(L *lua.LState) {
	mod := L.NewTable()
	mod.RawSetString("charpattern", lua.LString("[\x00-\x7f\xc2-\xf4][\x80-\xbf]*"))
	mod.RawSetString("char", L.NewFunction(utf8Char))
	mod.RawSetString("len", L.NewFunction(utf8Len))
	mod.RawSetString("codepoint", L.NewFunction(utf8Codepoint))
	mod.RawSetString("codes", L.NewFunction(utf8Codes))
	mod.RawSetString("offset", L.NewFunction(utf8Offset))
	L.SetGlobal("utf8", mod)
}

func utf8Char(L *lua.LState) int {
	n := L.GetTop()
	var b strings.Builder
	b.Grow(n)
	var buf [utf8.UTFMax]byte
	for i := 1; i <= n; i++ {
		cp := L.CheckInt(i)
		if cp < 0 || cp > 0x10FFFF {
			L.ArgError(i, "value out of range")
			return 0
		}
		// Match Lua 5.3 / Luau utf8.char: encode valid Unicode scalar values;
		// surrogates (0xD800..0xDFFF) are rejected as invalid UTF-8 by Go's
		// EncodeRune (which emits U+FFFD), matching standard UTF-8 safety.
		sz := utf8.EncodeRune(buf[:], rune(cp))
		b.Write(buf[:sz])
	}
	L.Push(lua.LString(b.String()))
	return 1
}

func utf8RelPos(pos, length int) int {
	if pos >= 0 {
		return pos
	}
	if -pos > length {
		return 0
	}
	return length + pos + 1
}

func utf8Len(L *lua.LState) int {
	s := L.CheckString(1)
	length := len(s)
	i := utf8RelPos(L.OptInt(2, 1), length)
	j := utf8RelPos(L.OptInt(3, -1), length)
	if i < 1 || i > length+1 {
		L.ArgError(2, "initial position out of string")
		return 0
	}
	if j > length {
		L.ArgError(3, "final position out of string")
		return 0
	}
	if i > j {
		L.Push(lua.LNumber(0))
		return 1
	}
	sub := s[i-1 : j]
	count := 0
	for pos := 0; pos < len(sub); {
		r, sz := utf8.DecodeRuneInString(sub[pos:])
		if r == utf8.RuneError && sz == 1 {
			L.Push(lua.LNil)
			L.Push(lua.LNumber(i + pos))
			return 2
		}
		count++
		pos += sz
	}
	L.Push(lua.LNumber(count))
	return 1
}

func utf8Codepoint(L *lua.LState) int {
	s := L.CheckString(1)
	length := len(s)
	i := utf8RelPos(L.OptInt(2, 1), length)
	j := utf8RelPos(L.OptInt(3, i), length)
	if i < 1 {
		L.ArgError(2, "out of range")
		return 0
	}
	if j > length {
		L.ArgError(3, "out of range")
		return 0
	}
	if i > j {
		return 0
	}
	n := 0
	for pos := i - 1; pos < j; {
		r, sz := utf8.DecodeRuneInString(s[pos:])
		if r == utf8.RuneError && sz == 1 {
			L.RaiseError("invalid UTF-8 code")
			return 0
		}
		L.Push(lua.LNumber(r))
		n++
		pos += sz
	}
	return n
}

func utf8CodesIter(L *lua.LState) int {
	s := L.CheckString(1)
	pos := L.CheckInt(2)
	if pos <= 0 {
		pos = 1
	} else {
		// Advance past the current rune starting at 1-based pos.
		if pos > len(s) {
			return 0
		}
		_, sz := utf8.DecodeRuneInString(s[pos-1:])
		pos += sz
	}
	if pos > len(s) {
		return 0
	}
	r, sz := utf8.DecodeRuneInString(s[pos-1:])
	if r == utf8.RuneError && sz == 1 {
		L.RaiseError("invalid UTF-8 code")
		return 0
	}
	L.Push(lua.LNumber(pos))
	L.Push(lua.LNumber(r))
	return 2
}

func utf8Codes(L *lua.LState) int {
	s := L.CheckString(1)
	L.Push(L.NewFunction(utf8CodesIter))
	L.Push(lua.LString(s))
	L.Push(lua.LNumber(0))
	return 3
}

func utf8Offset(L *lua.LState) int {
	s := L.CheckString(1)
	n := L.CheckInt(2)
	length := len(s)
	defI := 1
	if n < 0 {
		defI = length + 1
	}
	i := utf8RelPos(L.OptInt(3, defI), length)
	if i < 1 || i > length+1 {
		L.ArgError(3, "position out of range")
		return 0
	}
	pos := i - 1 // 0-based
	if n == 0 {
		for pos > 0 && !utf8.RuneStart(s[pos]) {
			pos--
		}
		L.Push(lua.LNumber(pos + 1))
		return 1
	}
	if pos < length && !utf8.RuneStart(s[pos]) {
		L.RaiseError("initial position is a continuation byte")
		return 0
	}
	if n > 0 {
		n-- // n=1 means current character
		for n > 0 && pos < length {
			_, sz := utf8.DecodeRuneInString(s[pos:])
			pos += sz
			n--
		}
	} else {
		for n < 0 && pos > 0 {
			pos--
			for pos > 0 && !utf8.RuneStart(s[pos]) {
				pos--
			}
			n++
		}
	}
	if n == 0 {
		L.Push(lua.LNumber(pos + 1))
	} else {
		L.Push(lua.LNil)
	}
	return 1
}

// ----------------------------------------------------------------- bit32 ---

func openBit32(L *lua.LState) {
	mod := L.NewTable()
	mod.RawSetString("band", L.NewFunction(func(L *lua.LState) int {
		res := ^uint32(0)
		for i := 1; i <= L.GetTop(); i++ {
			res &= uint32(L.CheckInt64(i))
		}
		L.Push(lua.LNumber(res))
		return 1
	}))
	mod.RawSetString("bor", L.NewFunction(func(L *lua.LState) int {
		var res uint32
		for i := 1; i <= L.GetTop(); i++ {
			res |= uint32(L.CheckInt64(i))
		}
		L.Push(lua.LNumber(res))
		return 1
	}))
	mod.RawSetString("bxor", L.NewFunction(func(L *lua.LState) int {
		var res uint32
		for i := 1; i <= L.GetTop(); i++ {
			res ^= uint32(L.CheckInt64(i))
		}
		L.Push(lua.LNumber(res))
		return 1
	}))
	mod.RawSetString("bnot", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(^uint32(L.CheckInt64(1))))
		return 1
	}))
	mod.RawSetString("btest", L.NewFunction(func(L *lua.LState) int {
		res := ^uint32(0)
		for i := 1; i <= L.GetTop(); i++ {
			res &= uint32(L.CheckInt64(i))
		}
		L.Push(lua.LBool(res != 0))
		return 1
	}))
	mod.RawSetString("lshift", L.NewFunction(func(L *lua.LState) int {
		x := uint32(L.CheckInt64(1))
		disp := L.CheckInt(2)
		var res uint32
		if disp >= 0 && disp < 32 {
			res = x << uint(disp)
		} else if disp < 0 && -disp < 32 {
			res = x >> uint(-disp)
		}
		L.Push(lua.LNumber(res))
		return 1
	}))
	mod.RawSetString("rshift", L.NewFunction(func(L *lua.LState) int {
		x := uint32(L.CheckInt64(1))
		disp := L.CheckInt(2)
		var res uint32
		if disp >= 0 && disp < 32 {
			res = x >> uint(disp)
		} else if disp < 0 && -disp < 32 {
			res = x << uint(-disp)
		}
		L.Push(lua.LNumber(res))
		return 1
	}))
	mod.RawSetString("arshift", L.NewFunction(func(L *lua.LState) int {
		x := uint32(L.CheckInt64(1))
		disp := L.CheckInt(2)
		var res uint32
		if disp < 0 {
			if -disp < 32 {
				res = x << uint(-disp)
			}
		} else if disp >= 32 {
			if int32(x) < 0 {
				res = ^uint32(0)
			}
		} else {
			res = uint32(int32(x) >> uint(disp))
		}
		L.Push(lua.LNumber(res))
		return 1
	}))
	mod.RawSetString("lrotate", L.NewFunction(func(L *lua.LState) int {
		x := uint32(L.CheckInt64(1))
		disp := L.CheckInt(2)
		L.Push(lua.LNumber(bits.RotateLeft32(x, disp)))
		return 1
	}))
	mod.RawSetString("rrotate", L.NewFunction(func(L *lua.LState) int {
		x := uint32(L.CheckInt64(1))
		disp := L.CheckInt(2)
		L.Push(lua.LNumber(bits.RotateLeft32(x, -disp)))
		return 1
	}))
	mod.RawSetString("extract", L.NewFunction(func(L *lua.LState) int {
		x := uint32(L.CheckInt64(1))
		field := L.CheckInt(2)
		width := L.OptInt(3, 1)
		if field < 0 || width <= 0 || field+width > 32 {
			L.RaiseError("trying to access non-existent bits")
			return 0
		}
		mask := uint32((uint64(1) << uint(width)) - 1)
		L.Push(lua.LNumber((x >> uint(field)) & mask))
		return 1
	}))
	mod.RawSetString("replace", L.NewFunction(func(L *lua.LState) int {
		x := uint32(L.CheckInt64(1))
		v := uint32(L.CheckInt64(2))
		field := L.CheckInt(3)
		width := L.OptInt(4, 1)
		if field < 0 || width <= 0 || field+width > 32 {
			L.RaiseError("trying to access non-existent bits")
			return 0
		}
		mask := uint32((uint64(1) << uint(width)) - 1)
		res := (x & ^(mask << uint(field))) | ((v & mask) << uint(field))
		L.Push(lua.LNumber(res))
		return 1
	}))
	mod.RawSetString("countlz", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(bits.LeadingZeros32(uint32(L.CheckInt64(1)))))
		return 1
	}))
	mod.RawSetString("countrz", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(bits.TrailingZeros32(uint32(L.CheckInt64(1)))))
		return 1
	}))
	L.SetGlobal("bit32", mod)
}
