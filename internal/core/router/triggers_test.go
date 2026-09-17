package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/caps"
	"agentflow/internal/core/session"
)

// chanHandler forwards log records to a channel so a test can observe what a
// route logged without racing on a shared buffer.
type chanHandler struct{ ch chan string }

func (h chanHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h chanHandler) Handle(_ context.Context, r slog.Record) error {
	select {
	case h.ch <- r.Message:
	default:
	}
	return nil
}
func (h chanHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h chanHandler) WithGroup(string) slog.Handler      { return h }

// waitLine reads log lines until one matches prefix (the router also logs
// lifecycle lines like "router started").
func waitLine(t *testing.T, ch <-chan string, prefix string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-ch:
			if strings.HasPrefix(line, prefix) {
				return line
			}
		case <-deadline:
			t.Fatalf("no log line with prefix %q", prefix)
		}
	}
}

// TestRouterAnswersRuntimeTriggers: route Lua can read the deployment's
// trigger list through runtime.triggers(), answered by the router op switch
// with the same payload the agent-facing handler returns.
func TestRouterAnswersRuntimeTriggers(t *testing.T) {
	triggers := []config.Trigger{
		{Name: "ticket", Event: &config.TriggerEvent{Channel: "webhook", Match: "urgent"},
			Target: config.TriggerTarget{Profile: "worker"}},
		{Name: "digest", Cron: "0 9 * * *", RunOnBoot: true,
			Target: config.TriggerTarget{Profile: "worker"}, Payload: map[string]any{"topic": "news"}},
	}

	ch := make(chan string, 8)
	r := New(`
function loop()
  while true do
    local item = session.inbox()
    log.info("TRIGGERS:" .. json.encode(runtime.triggers()))
  end
end
`, caps.TriggersResponse(triggers), nil, slog.New(chanHandler{ch: ch}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	r.Submit(Inbound{Channel: "webhook", Agent: "main",
		Message: session.Message{ID: "m1", Type: "user", From: "u", Text: "go"}})

	line := waitLine(t, ch, "TRIGGERS:")
	body, ok := strings.CutPrefix(line, "TRIGGERS:")
	if !ok {
		t.Fatalf("unexpected log line: %q", line)
	}

	// Structural equality with the agent-facing op: same bytes in, same data
	// out (the prelude re-encodes, so key order is not comparable).
	var fromRouter, fromAgent struct {
		OK       bool             `json:"ok"`
		Triggers []map[string]any `json:"triggers"`
	}
	if err := json.Unmarshal([]byte(body), &fromRouter); err != nil {
		t.Fatalf("router response is not JSON (%v): %s", err, body)
	}
	if err := json.Unmarshal([]byte(caps.TriggersResponse(triggers)), &fromAgent); err != nil {
		t.Fatal(err)
	}
	if !fromRouter.OK || len(fromRouter.Triggers) != 2 {
		t.Fatalf("router payload wrong: %s", body)
	}
	if !equalJSON(t, fromRouter.Triggers, fromAgent.Triggers) {
		t.Fatalf("router payload differs from the agent handler:\n router=%s\n  agent=%s",
			body, caps.TriggersResponse(triggers))
	}

	// The event route — the motivating case — is readable.
	first := fromRouter.Triggers[0]
	if first["name"] != "ticket" || first["kind"] != "event" {
		t.Fatalf("event trigger wrong: %v", first)
	}
}

// equalJSON compares two decoded JSON values structurally.
func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return normalizeJSON(t, ab) == normalizeJSON(t, bb)
}

// normalizeJSON canonicalizes a decoded JSON value for comparison: key order
// is fixed by re-marshaling through a map, and null-valued keys are dropped
// because a Lua round-trip cannot distinguish null from an absent key —
// json.decode yields nil for both and the prelude's encoder omits nil, so the
// router's re-encoded payload legitimately lacks keys the Go builder wrote as
// null. Lua-visible data is identical either way.
func normalizeJSON(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(dropNulls(v))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func dropNulls(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if val == nil {
				delete(t, k)
				continue
			}
			t[k] = dropNulls(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = dropNulls(val)
		}
		return t
	default:
		return v
	}
}

// TestRouterTriggersDefaultsEmpty: a router built without a payload answers an
// empty trigger list rather than erroring the op.
func TestRouterTriggersDefaultsEmpty(t *testing.T) {
	ch := make(chan string, 8)
	r := New(`
function loop()
  while true do
    local item = session.inbox()
    local t = runtime.triggers()
    log.info("N:" .. tostring(#t.triggers))
  end
end
`, "", nil, slog.New(chanHandler{ch: ch}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	r.Submit(Inbound{Channel: "webhook", Agent: "main", Message: session.Message{ID: "m2", Type: "user"}})

	line := waitLine(t, ch, "N:")
	if line != "N:0" {
		t.Fatalf("expected an empty trigger list, got %q", line)
	}
}
