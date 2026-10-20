package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
	"agentflow/internal/core/studio"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/vm"
)

// studioFixture builds a UI with a real workspace, a supervisor holding one
// echo agent, and a broker whose router simulates a turn.
type studioFixture struct {
	ui     *UI
	dir    string
	sup    *supervisor.Supervisor
	broker *studio.Broker
	router *testRouter
	ws     *studio.Workspace
}

type testRouter struct {
	onDeliver func(agent, key string, msg session.Message)
}

func (f *testRouter) Deliver(agent, key string, msg session.Message) error {
	if f.onDeliver != nil {
		f.onDeliver(agent, key, msg)
	}
	return nil
}
func (f *testRouter) StopSession(string)              {}
func (f *testRouter) SessionBusy(string) (bool, bool) { return false, true }

func newStudioFixture(t *testing.T) *studioFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "system.yaml"), []byte("version: \"1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profiles", "base.yaml"), []byte("name: base\nloop: ./base.lua\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A minimal runtime config for validation (ValidateAgent only needs the
	// capability ceiling and models).
	cfg, err := config.LoadDir(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("fixture configdir does not load: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup := supervisor.New(map[string]*supervisor.AgentDef{}, gateway.NewRegistry(log), pool.New(1), nil, log)
	sup.Start(context.Background())

	r := &testRouter{}
	b := studio.NewBroker(r, nil, studio.Config{}, log)
	r.onDeliver = func(agent, key string, msg session.Message) {
		sk := agent + "|" + key
		b.Trace(session.TraceEvent{SessionID: sk, Kind: "turn.start", Turn: msg.ID})
		_ = b.Deliver(msg.ReplyTo, "echo: "+msg.Text, nil)
		b.Trace(session.TraceEvent{SessionID: sk, Kind: "turn.end", Turn: msg.ID})
	}

	// The playground needs a configured agent definition to target.
	if err := sup.UpsertAgent("echo", &supervisor.AgentDef{
		Info:     &session.Info{Name: "echo"},
		Handlers: map[string]session.OpHandler{},
	}); err != nil {
		t.Fatal(err)
	}

	ws := studio.NewWorkspace(dir, "", "", cfg, log)
	ui := New(Deps{
		ConfigDir: dir,
		Cfg:       cfg,
		Agents:    sup,
		BuildAgent: func(name string, a config.Agent) (*supervisor.AgentDef, error) {
			return &supervisor.AgentDef{
				Info:         &session.Info{Name: name, Model: a.Model},
				LoopFile:     a.Loop,
				Handlers:     map[string]session.OpHandler{},
				Capabilities: map[string]bool{},
			}, nil
		},
		Studio:    b,
		Workspace: ws,
		StudioCatalog: func() studio.Catalog {
			return studio.NewCatalog([]string{"default"}, []string{"builtin:fetch"}, []string{"default", "none"}, nil, nil, []string{"tools"})
		},
	})
	return &studioFixture{ui: ui, dir: dir, sup: sup, broker: b, router: r, ws: ws}
}

func testLoopBody() string {
	return fmt.Sprintf("-- af-prelude-version: %d\nfunction loop()\n  local m = session.inbox()\n  session.send(m.text)\nend\n", vm.PreludeVersion)
}

func TestStudioCatalogAndValidate(t *testing.T) {
	f := newStudioFixture(t)
	rec := do(t, f.ui.API(), "GET", "/admin/api/studio/catalog", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"builtin:fetch"`) {
		t.Fatalf("catalog = %d %s", rec.Code, rec.Body.String())
	}

	body := map[string]any{
		"name":       "lab",
		"agent_yaml": "loop: ./lab.lua\ncapabilities: [tools]\n",
		"loop":       testLoopBody(),
	}
	rec = do(t, f.ui.API(), "POST", "/admin/api/studio/agents/validate", body)
	var v struct {
		OK     bool     `json:"ok"`
		Errors []string `json:"errors"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if !v.OK {
		t.Fatalf("valid agent rejected: %v", v.Errors)
	}

	body["loop"] = "function loop() end\n" // no prelude directive
	rec = do(t, f.ui.API(), "POST", "/admin/api/studio/agents/validate", body)
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v.OK {
		t.Fatal("loop without a prelude directive accepted")
	}
}

func TestStudioApplyPersistAndDelete(t *testing.T) {
	f := newStudioFixture(t)
	body := map[string]any{
		"agent_yaml":   "loop: ./lab.lua\ncapabilities: [tools]\n",
		"loop":         testLoopBody(),
		"instructions": "Be terse.",
		"persist":      true,
	}
	rec := do(t, f.ui.API(), "PUT", "/admin/api/studio/agents/lab", body)
	if rec.Code != 200 {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body.String())
	}
	var v struct {
		OK        bool `json:"ok"`
		Applied   bool `json:"applied"`
		Persisted bool `json:"persisted"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if !v.OK || !v.Applied || !v.Persisted {
		t.Fatalf("apply response = %+v (%s)", v, rec.Body.String())
	}
	if _, ok := f.sup.Agents()["lab"]; !ok {
		t.Fatal("agent not installed in the runtime")
	}
	if !f.ws.Persisted("lab") {
		t.Fatal("agent not persisted")
	}

	rec = do(t, f.ui.API(), "GET", "/admin/api/studio/agents/lab", nil)
	if rec.Code != 200 {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Loop         string `json:"loop"`
		Instructions string `json:"instructions"`
		Source       string `json:"source"`
		Persisted    bool   `json:"persisted"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if !strings.Contains(detail.Loop, "function loop") || detail.Instructions != "Be terse." || !detail.Persisted {
		t.Fatalf("detail = %+v", detail)
	}

	rec = do(t, f.ui.API(), "DELETE", "/admin/api/studio/agents/lab?persist=true", nil)
	if rec.Code != 200 {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := f.sup.Agents()["lab"]; ok {
		t.Fatal("agent still in the runtime")
	}
	if _, _, _, _, err := f.ws.Load("lab"); err == nil {
		t.Fatal("agent still persisted")
	}
}

// TestStudioGetPrefersTheLiveEdit: the boot config is a startup snapshot, so a
// reload after Apply must show what was applied, not the pre-edit definition.
func TestStudioGetPrefersTheLiveEdit(t *testing.T) {
	f := newStudioFixture(t)
	body := map[string]any{
		"agent_yaml": "loop: ./base.lua\ncapabilities: [tools, llm.chat]\n",
		"loop":       testLoopBody(),
		"persist":    false,
	}
	rec := do(t, f.ui.API(), "PUT", "/admin/api/studio/agents/base", body)
	if rec.Code != 200 {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, f.ui.API(), "GET", "/admin/api/studio/agents/base", nil)
	var detail struct {
		Capabilities []string `json:"capabilities"`
		Source       string   `json:"source"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	joined := strings.Join(detail.Capabilities, ",")
	if !strings.Contains(joined, "llm.chat") || detail.Source != "runtime" {
		t.Fatalf("reload after apply = %+v, want the applied definition", detail)
	}
}

func TestStudioChatNonStream(t *testing.T) {
	f := newStudioFixture(t)
	rec := do(t, f.ui.API(), "POST", "/admin/api/studio/chat", map[string]any{
		"agent":  "echo",
		"thread": "t1",
		"text":   "hi",
		"stream": false,
	})
	if rec.Code != 200 {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body.String())
	}
	var v struct {
		OK     bool `json:"ok"`
		Result struct {
			Thread  string `json:"thread"`
			Replies []struct {
				Text string `json:"text"`
			} `json:"replies"`
		} `json:"result"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if !v.OK || len(v.Result.Replies) != 1 || v.Result.Replies[0].Text != "echo: hi" || v.Result.Thread != "t1" {
		t.Fatalf("chat response = %s", rec.Body.String())
	}

	// The trace buffered for the thread is readable.
	rec = do(t, f.ui.API(), "GET", "/admin/api/studio/trace/echo/t1", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"turn.start"`) {
		t.Fatalf("trace = %d %s", rec.Code, rec.Body.String())
	}
}

func TestStudioChatRejectsUnknownAgent(t *testing.T) {
	f := newStudioFixture(t)
	rec := do(t, f.ui.API(), "POST", "/admin/api/studio/chat", map[string]any{"agent": "nope", "text": "hi"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("chat unknown agent = %d", rec.Code)
	}
}

func TestStudioChatSSE(t *testing.T) {
	f := newStudioFixture(t)
	req := httptest.NewRequest("POST", "/admin/api/studio/chat", strings.NewReader(`{"agent":"echo","thread":"t2","text":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.ui.API().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "event: reply") || !strings.Contains(body, "event: done") {
		t.Fatalf("SSE body = %q", body)
	}
}

func TestStudioDisabled(t *testing.T) {
	ui := New(Deps{Cfg: &config.Config{}})
	rec := do(t, ui.API(), "GET", "/admin/api/studio/agents", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled studio = %d", rec.Code)
	}
}
