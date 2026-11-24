package vm

import (
	"encoding/json"
	"fmt"
	"testing"
)

func driveStream(t *testing.T, body string) (Status, string, bool) {
	t.Helper()
	st := New(5_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf("// af-prelude-version: %d\n%s", PreludeVersion, body)
	status, msg := st.Start("run", "@stream-test", src)
	frames := []string{
		`{"delta":"a","done":false}`,
		`{"delta":"b","done":false}`,
		`{"done":true}`,
	}
	frame := 0
	closedCalled := false
	for status == Yielded {
		var req struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(msg), &req); err != nil {
			t.Fatalf("yielded request is not JSON: %v (%q)", err, msg)
		}
		switch req.Type {
		case "llm.stream.open":
			status, msg = st.Resume(`{"id":"s1"}`, true)
		case "llm.stream.next":
			if frame >= len(frames) {
				t.Fatalf("read past the last frame (request %q)", msg)
			}
			status, msg = st.Resume(frames[frame], true)
			frame++
		case "llm.stream.close":
			closedCalled = true
			status, msg = st.Resume("true", true)
		default:
			t.Fatalf("unexpected op %q", req.Type)
		}
	}
	return status, msg, closedCalled
}

func TestLLMStreamForOfWorksNatively(t *testing.T) {
	status, msg, closed := driveStream(t, `
function run(): void {
  const out: string[] = [];
  for (const d of llm.stream([{ role: "user", content: "hi" }])) {
    out.push(d);
  }
  if (out.join(",") !== "a,b") {
    throw new Error("got: " + out.join(","));
  }
}
`)
	if status != Finished || !closed {
		t.Fatalf("for..of stream failed: status=%v msg=%q closed=%v", status, msg, closed)
	}
}

func TestLLMStreamEarlyBreakClosesStreamAutomatically(t *testing.T) {
	status, msg, closed := driveStream(t, `
function run(): void {
  const out: string[] = [];
  for (const d of llm.stream([{ role: "user", content: "hi" }])) {
    out.push(d);
    break;
  }
  if (out.join(",") !== "a") {
    throw new Error("got: " + out.join(","));
  }
}
`)
	if status != Finished || !closed {
		t.Fatalf("early-break stream did not auto-close: status=%v msg=%q closed=%v", status, msg, closed)
	}
}

func TestLLMStreamManualNextAndClose(t *testing.T) {
	status, msg, closed := driveStream(t, `
function run(): void {
  const out: string[] = [];
  const s = llm.stream([{ role: "user", content: "hi" }]);
  while (true) {
    const step = s.next();
    if (step.done) break;
    out.push(step.value);
  }
  s.close();
  if (out.join(",") !== "a,b") {
    throw new Error("got: " + out.join(","));
  }
}
`)
	if status != Finished || !closed {
		t.Fatalf("manual stream failed: status=%v msg=%q closed=%v", status, msg, closed)
	}
}
