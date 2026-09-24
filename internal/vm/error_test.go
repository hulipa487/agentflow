package vm

import (
	"strings"
	"testing"
)

// luaErrorAfterYield drives a state onto the resume path and returns the
// failure, so these assertions are about afvm_lastmsg rather than afvm_start's
// own error buffer.
func luaErrorAfterYield(t *testing.T, raise string) (Status, string) {
	t.Helper()
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if status, msg := st.Start("loop", "session.inbox()\n"+raise+"\n"); status != Yielded {
		t.Fatalf("expected the chunk to yield an op first, got %v: %s", status, msg)
	}
	return st.Resume(`{"ok":true}`, true)
}

// TestErrorMessagesSurviveTheResumePath: afvm_resume has no error buffer of its
// own — it reports through afvm_lastmsg — and that reader used lua_tolstring,
// which returns NULL for anything but a string or a number. A loop raising a
// table therefore surfaced as `loop error err=""`, and the cause was lost
// exactly when it was least obvious.
func TestErrorMessagesSurviveTheResumePath(t *testing.T) {
	status, msg := luaErrorAfterYield(t, `error({ code = 1 })`)
	if status != Failed {
		t.Fatalf("status = %v; want Failed", status)
	}
	if msg == "" {
		t.Fatal("a non-string error surfaced as an empty message")
	}
	if !strings.Contains(msg, "non-string error") || !strings.Contains(msg, "table") {
		t.Fatalf("message %q does not name the non-string error", msg)
	}

	// A string error must still carry its own text, unchanged.
	status, msg = luaErrorAfterYield(t, `error("boom")`)
	if status != Failed {
		t.Fatalf("status = %v; want Failed", status)
	}
	if !strings.Contains(msg, "boom") {
		t.Fatalf("message %q lost the error text", msg)
	}
}
