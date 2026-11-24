package looptest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEchoScenario(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "echo.ts"), `// af-prelude-version: 2
function loop() {
  while (true) {
    const msg = session.inbox();
    session.send("echo: " + (msg.text || ""));
  }
}
`)
	scenario := filepath.Join(dir, "scenario.yaml")
	write(t, scenario, `agent: echo
loop: ./echo.ts
messages:
  - from: alice
    text: hi
expect:
  - "echo: hi"
`)
	sc, err := LoadFile(scenario, mustRead(t, scenario))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(sc, &out); err != nil {
		t.Fatal(err)
	}
}

func TestLLMScenario(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "chat.ts"), `// af-prelude-version: 2
function loop() {
  while (true) {
    const msg = session.inbox();
    const reply = llm.chat([{ role: "user", content: msg.text }], { model: "test" });
    session.send(reply.text);
  }
}
`)
	scenario := filepath.Join(dir, "scenario.yaml")
	write(t, scenario, `agent: chat
loop: ./chat.ts
llm:
  - match: refund
    text: I can help.
messages:
  - from: alice
    text: I need a refund
expect:
  - I can help.
`)
	sc, err := LoadFile(scenario, mustRead(t, scenario))
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(sc, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPackageScenario(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "pkg", "manifest.yaml"), "name: greet\nprelude_api: 2\nfiles: [greet.ts]\n")
	write(t, filepath.Join(dir, "pkg", "greet.ts"), `// af-prelude-version: 2
var Greeting = {
  hello(name) { return "hello, " + name; }
};
`)
	write(t, filepath.Join(dir, "loop.ts"), `// af-prelude-version: 2
// af-import: greet
function loop() {
  while (true) {
    const msg = session.inbox();
    session.send(Greeting.hello(msg.text));
  }
}
`)
	scenario := filepath.Join(dir, "scenario.yaml")
	write(t, scenario, `agent: pkg
loop: ./loop.ts
packages:
  greet: ./pkg
messages:
  - from: alice
    text: Ada
expect:
  - "hello, Ada"
`)
	sc, err := LoadFile(scenario, mustRead(t, scenario))
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(sc, nil); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRunWithResultCapturesTraceUsageAndAssertions(t *testing.T) {
	sc := &Scenario{
		Agent: "eval-agent",
		LoopSource: `// af-prelude-version: 2
function loop() {
  while (true) {
    const msg = session.inbox();
    const reply = llm.chat([{ role: "user", content: msg.text || "" }], { model: "test-model" });
    session.send(reply.text || "");
  }
}
`,
		Safety: "default",
		LLM: []LLMReply{
			{
				Match: "help me right now",
				Text:  "Take a slow breath.",
				Usage: &LLMUsage{Input: 12, Output: 7, Reasoning: 2},
			},
		},
		Messages:       []Input{{From: "alice", Text: "help me right now"}},
		ExpectContains: []string{"Take a slow breath."},
		ExpectOps:      []string{"llm.chat"},
	}

	res, err := RunWithResult(context.Background(), sc)
	if err != nil {
		t.Fatalf("RunWithResult: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected OK=true, got err=%q assertions=%+v", res.Error, res.Assertions)
	}
	if len(res.Sends) != 1 || !strings.Contains(res.Sends[0], "local support helpline") {
		t.Fatalf("expected default safety SupportOffer append, got sends=%v", res.Sends)
	}
	if len(res.Assertions) != 2 || !res.Assertions[0].OK || !res.Assertions[1].OK {
		t.Fatalf("unexpected assertions: %+v", res.Assertions)
	}

	var sawStart, sawOpUsage, sawSafety, sawEnd bool
	for _, ev := range res.Events {
		switch {
		case ev.Kind == "turn.start":
			sawStart = true
		case ev.Kind == "op.end" && ev.Op == "llm.chat":
			sawOpUsage = ev.TokensIn == 12 && ev.TokensOut == 7 && ev.TokensReasoning == 2
		case ev.Kind == "safety":
			sawSafety = true
		case ev.Kind == "turn.end":
			sawEnd = true
		}
	}
	if !sawStart || !sawOpUsage || !sawSafety || !sawEnd {
		t.Fatalf("missing expected trace events: start=%v opUsage=%v safety=%v end=%v (%+v)", sawStart, sawOpUsage, sawSafety, sawEnd, res.Events)
	}

	// Failing assertion populates res.OK=false with per-assertion failure info.
	sc.Expect = []string{"something completely different"}
	failRes, err := RunWithResult(context.Background(), sc)
	if err != nil {
		t.Fatalf("RunWithResult failing case: %v", err)
	}
	if failRes.OK || failRes.Error == "" {
		t.Fatalf("expected failing result, got %+v", failRes)
	}
}
