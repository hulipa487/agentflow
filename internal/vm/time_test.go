package vm

import (
	"fmt"
	"testing"
	"time"
)

func TestSandboxClockIsUTCOnly(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}

	before := time.Now().Unix()
	if err := st.Eval("@time", fmt.Sprintf(`(function() {
const assert = (c, m) => { if (!c) throw new Error(m || "assertion failed"); };
const a = os.time();
assert(typeof a === "number", "os.time must return a number");
assert(a === Math.floor(a), "os.time must be whole seconds, got " + String(a));
assert(a >= %d && a <= %d, "os.time outside window: " + String(a));
assert(os.time() >= a, "os.time must not go backwards");
})();`, before, before+5)); err != nil {
		t.Fatalf("os.time contract broken: %v", err)
	}

	if err := st.Eval("@timearg", `(function() {
let ok = false;
let errMsg = "";
try {
  os.time({});
  ok = true;
} catch (e) {
  errMsg = String(e);
}
if (ok) throw new Error("os.time({}) must fail");
if (!errMsg.includes("takes no arguments")) throw new Error("unclear error: " + errMsg);
})();`); err != nil {
		t.Fatalf("os.time argument handling wrong: %v", err)
	}

	if err := st.Eval("@date", `(function() {
const assert = (c, m) => { if (!c) throw new Error(m || "assertion failed"); };
const d = os.date("*t", 1700000000);
assert(d.year === 2023 && d.month === 11 && d.day === 14, "ymd wrong: " + json.encode(d));
assert(d.hour === 22 && d.min === 13 && d.sec === 20, "hms wrong: " + json.encode(d));
assert(d.wday === 3, "wday must be 3 (Tuesday, 1=Sunday), got " + String(d.wday));
assert(d.yday === 318, "yday wrong: " + String(d.yday));
assert(d.isdst === false, "UTC has no daylight saving");

const e = os.date(0);
assert(e.year === 1970 && e.month === 1 && e.day === 1 && e.wday === 5 && e.yday === 1,
  "epoch wrong: " + json.encode(e));

const feb = os.date(951782400);
assert(feb.year === 2000 && feb.month === 2 && feb.day === 29, "leap day wrong: " + json.encode(feb));

assert(os.date(1700000000).day === 14, "os.date(time) must return the object");
assert(os.date("!*t", 1700000000).day === 14, "!*t must be accepted");

assert(os.date("%Y-%m-%d %H:%M:%S", 1700000000) === "2023-11-14 22:13:20",
  "got " + os.date("%Y-%m-%d %H:%M:%S", 1700000000));
assert(os.date("!%Y-%m-%d", 1700000000) === "2023-11-14", "! prefix must format");
assert(os.date("%A %a %B %b %j %w %u %Z", 1700000000) === "Tuesday Tue November Nov 318 2 2 UTC", "strftime mismatch");
assert(os.date("%e|%I|%p|%y|%%", 1700000000) === "14|10|PM|23|%", "strftime mismatch");
assert(os.date("%e", 0) === " 1", "space-padded day wrong");
assert(os.date("%q", 1700000000) === "%q", "unknown directive must stay verbatim");

const now = os.date();
assert(now.year >= 2024 && now.month >= 1 && now.month <= 12, "os.date() implausible");
assert(typeof os.date() === "object" && typeof os.date("%Y") === "string", "return types");
assert(json.encode(now).includes('"year"'), "date object must encode");

let failed = false;
try { os.date("*t", "tomorrow"); } catch (_) { failed = true; }
assert(failed, "os.date with string time must fail");
})();`); err != nil {
		t.Fatalf("os.date contract broken: %v", err)
	}

	if err := st.Eval("@osrest", `(function() {
const assert = (c, m) => { if (!c) throw new Error(m || "assertion failed"); };
assert(typeof os.env === "function", "os.env must survive");
assert(os.getenv === undefined && os.exit === undefined && os.remove === undefined
  && os.clock === undefined && os.execute === undefined, "unexpected os.* exposed");
assert(typeof globalThis.io === "undefined", "io must not exist");
assert(typeof globalThis.process === "undefined", "process must not exist");
assert(typeof globalThis.require === "undefined", "require must not exist");
assert(typeof __af_now === "function", "__af_now must be an inherited global");
})();`); err != nil {
		t.Fatalf("sandbox surface leaked: %v", err)
	}
}
