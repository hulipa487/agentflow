package vm

import (
	"strings"
	"testing"
)

func scriptErrorAfterYield(t *testing.T, raise string) (Status, string) {
	t.Helper()
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if status, msg := st.Start("loop", "test chunk", directive()+"session.inbox();\n"+raise+"\nfunction loop() {}\n"); status != Yielded {
		t.Fatalf("expected the chunk to yield an op first, got %v: %s", status, msg)
	}
	return st.Resume(`{"ok":true}`, true)
}

func TestErrorMessagesSurviveTheResumePath(t *testing.T) {
	status, msg := scriptErrorAfterYield(t, `throw { code: 1 };`)
	if status != Failed {
		t.Fatalf("status = %v; want Failed", status)
	}
	if msg == "" {
		t.Fatal("a non-string error surfaced as an empty message")
	}
	if !strings.Contains(msg, "non-string error") || !strings.Contains(msg, `"code":1`) {
		t.Fatalf("message %q does not describe the non-string error", msg)
	}

	status, msg = scriptErrorAfterYield(t, `throw new Error("boom");`)
	if status != Failed {
		t.Fatalf("status = %v; want Failed", status)
	}
	if !strings.Contains(msg, "boom") {
		t.Fatalf("message %q lost the error text", msg)
	}
}
