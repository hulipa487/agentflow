package vm

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// drive runs one loop chunk, answering its ops the way the actor does: yield
// on each request, resume with a stream frame. It returns the terminal status
// and message.
func drive(t *testing.T, body string) (Status, string) {
	t.Helper()
	st := New(5_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf("-- af-prelude-version: %d\n%s", PreludeVersion, body)
	status, msg := st.Start("run", "@stream-test", src)
	frames := []string{
		`{"delta":"a","done":false}`,
		`{"delta":"b","done":false}`,
		`{"done":true}`,
	}
	frame := 0
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
			status, msg = st.Resume("true", true)
		default:
			t.Fatalf("unexpected op %q", req.Type)
		}
	}
	return status, msg
}

// TestLLMStreamDirectIteratorWorks: the documented form — call next_delta
// yourself — suspends and resumes across the bridge and returns every delta.
func TestLLMStreamDirectIteratorWorks(t *testing.T) {
	status, msg := drive(t, `
function run()
  local out = {}
  local next_delta, close = llm.stream({ { role = "user", content = "hi" } })
  while true do
    local d = next_delta()
    if d == nil then break end
    out[#out + 1] = d
  end
  close()
  assert(table.concat(out, ",") == "a,b", "got: " .. table.concat(out, ","))
end
`)
	if status != Finished {
		t.Fatalf("direct iterator failed: status=%v msg=%q", status, msg)
	}
}

// TestLLMStreamGenericForIsRefused pins the limitation that made the direct
// form necessary: a generic for calls the iterator from a C boundary, and the
// op's yield is refused there. If a Luau upgrade ever allows it, this test
// fails and the prelude comment can be relaxed.
func TestLLMStreamGenericForIsRefused(t *testing.T) {
	status, msg := drive(t, `
function run()
  for delta in llm.stream({ { role = "user", content = "hi" } }) do
    local _ = delta
  end
end
`)
	if status != Failed || !strings.Contains(msg, "yield across") {
		t.Fatalf("generic for = status %v, msg %q; want the yield-across refusal", status, msg)
	}
}
