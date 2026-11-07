package looptest

import (
	"bytes"
	"os"
	"path/filepath"
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
	write(t, filepath.Join(dir, "echo.lua"), `-- af-prelude-version: 1
function loop()
  while true do
    local msg = session.inbox()
    session.send("echo: " .. (msg.text or ""))
  end
end
`)
	scenario := filepath.Join(dir, "scenario.yaml")
	write(t, scenario, `agent: echo
loop: ./echo.lua
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
	write(t, filepath.Join(dir, "chat.lua"), `-- af-prelude-version: 1
function loop()
  while true do
    local msg = session.inbox()
    local reply = llm.chat({ { role = "user", content = msg.text } }, { model = "test" })
    session.send(reply.text)
  end
end
`)
	scenario := filepath.Join(dir, "scenario.yaml")
	write(t, scenario, `agent: chat
loop: ./chat.lua
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
	write(t, filepath.Join(dir, "pkg", "manifest.yaml"), "name: greet\nprelude_api: 1\nfiles: [greet.lua]\n")
	write(t, filepath.Join(dir, "pkg", "greet.lua"), `-- af-prelude-version: 1
Greeting = {}
function Greeting.hello(name) return "hello, " .. name end
`)
	write(t, filepath.Join(dir, "loop.lua"), `-- af-prelude-version: 1
-- af-import: greet
function loop()
  while true do
    local msg = session.inbox()
    session.send(Greeting.hello(msg.text))
  end
end
`)
	scenario := filepath.Join(dir, "scenario.yaml")
	write(t, scenario, `agent: pkg
loop: ./loop.lua
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
