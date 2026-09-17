package vm

import (
	"fmt"
	"testing"
	"time"
)

// The sandbox has no os library: os.env is a prelude op, and os.time/os.date
// are the prelude's UTC-only wall clock on top of the native __af_now(). This
// locks the field semantics Lua loops rely on (wday 1=Sunday, yday 1-366,
// isdst always false, whole seconds) and the fact that no other os.* leaked in.
func TestSandboxClockIsUTCOnly(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}

	// os.time() must land in [before, before+5] — the call happens inside the
	// eval, so a tight window that still tolerates a slow CI box.
	before := time.Now().Unix()
	if err := st.Eval("@time", fmt.Sprintf(`
local a = os.time()
assert(type(a) == "number", "os.time must return a number")
assert(a == math.floor(a), "os.time must be whole seconds, got " .. tostring(a))
assert(a >= %d and a <= %d, "os.time outside the expected window: " .. tostring(a))
assert(os.time() >= a, "os.time must not go backwards")
`, before, before+5)); err != nil {
		t.Fatalf("os.time contract broken: %v", err)
	}

	// An argument is an error, not a silent "now" (real Lua's os.time(table)
	// is not implemented here).
	if err := st.Eval("@timearg", `
local ok, err = pcall(os.time, {})
assert(not ok, "os.time({}) must fail")
assert(string.find(tostring(err), "takes no arguments", 1, true), "unclear error: " .. tostring(err))
`); err != nil {
		t.Fatalf("os.time argument handling wrong: %v", err)
	}

	// 1700000000 = 2023-11-14T22:13:20Z (a Tuesday, day 318 of the year).
	if err := st.Eval("@date", `
local d = os.date("*t", 1700000000)
assert(d.year == 2023 and d.month == 11 and d.day == 14, "ymd wrong: " .. json.encode(d))
assert(d.hour == 22 and d.min == 13 and d.sec == 20, "hms wrong: " .. json.encode(d))
assert(d.wday == 3, "wday must be 3 (Tuesday, 1=Sunday), got " .. tostring(d.wday))
assert(d.yday == 318, "yday wrong: " .. tostring(d.yday))
assert(d.isdst == false, "UTC has no daylight saving")
assert(d.wday >= 1 and d.wday <= 7 and d.yday >= 1 and d.yday <= 366, "field range")

-- the epoch itself: 1970-01-01 was a Thursday, day 1
local e = os.date(0)
assert(e.year == 1970 and e.month == 1 and e.day == 1 and e.wday == 5 and e.yday == 1,
       "epoch wrong: " .. json.encode(e))

-- the calendar must survive leap years and month ends
local feb = os.date(951782400)   -- 2000-02-29T00:00:00Z, a leap day (Tuesday)
assert(feb.year == 2000 and feb.month == 2 and feb.day == 29, "leap day wrong: " .. json.encode(feb))

-- os.date(t) with a bare number, and the "!*t" UTC marker, both give the table
assert(os.date(1700000000).day == 14, "os.date(time) must return the table")
assert(os.date("!*t", 1700000000).day == 14, "!*t must be accepted")

-- a format string gives a UTC-formatted string
assert(os.date("%Y-%m-%d %H:%M:%S", 1700000000) == "2023-11-14 22:13:20",
       "got " .. os.date("%Y-%m-%d %H:%M:%S", 1700000000))
assert(os.date("!%Y-%m-%d", 1700000000) == "2023-11-14", "! prefix must format")
assert(os.date("%A %a %B %b %j %w %u %Z", 1700000000)
       == "Tuesday Tue November Nov 318 2 2 UTC", "strftime mismatch")
assert(os.date("%e|%I|%p|%y|%%", 1700000000) == "14|10|PM|23|%", "strftime mismatch")
assert(os.date("%e", 0) == " 1", "space-padded day wrong")
assert(os.date("%q", 1700000000) == "%q", "unknown directive must stay verbatim")

-- no-argument forms use the clock and must be plausible
local now = os.date()
assert(now.year >= 2024 and now.month >= 1 and now.month <= 12, "os.date() implausible")
assert(type(os.date()) == "table" and type(os.date("%Y")) == "string", "return types")

-- the table is JSON-encodable (it crosses the bridge in real loops)
assert(string.find(json.encode(now), '"year"', 1, true), "date table must encode")

-- a non-number time is an error, never a silent "now"
local ok = pcall(os.date, "*t", "tomorrow")
assert(not ok, "os.date with a string time must fail")
`); err != nil {
		t.Fatalf("os.date contract broken: %v", err)
	}

	// Only os.env, os.time, os.date — no other os.* and no io/debug library.
	if err := st.Eval("@osrest", `
assert(type(os.env) == "function", "os.env must survive")
assert(os.getenv == nil and os.exit == nil and os.remove == nil and os.clock == nil
       and os.execute == nil and os.tmpname == nil, "unexpected os.* exposed")
assert(io == nil, "io must not exist")
assert(debug == nil, "debug must not exist")
assert(type(__af_now) == "function", "__af_now must be an inherited global")
`); err != nil {
		t.Fatalf("sandbox surface leaked: %v", err)
	}
}
