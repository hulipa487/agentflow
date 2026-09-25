package caps

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/media"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/llm"
)

// TestLLMChatResolvesAttachmentHandleToBase64 wires the full
// inbound→caps→provider path through the PRODUCTION LLMHandlers: a message
// lands with a blob-store image handle, a loop forwards it into llm.chat, and
// the caps layer resolves the handle to inline base64 so the provider sees a
// data URI. This is the core multimodal guarantee: bytes never cross the
// bridge, only the handle does. (It lives in package caps — not session —
// because caps imports session; the reverse would be a cycle. The session
// package used to carry a copy of the handler, which tested the copy.)
func TestLLMChatResolvesAttachmentHandleToBase64(t *testing.T) {
	// Provider endpoint asserts the request body carries the resolved data URI.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "data:image/png;base64,aGVsbG8=") {
			t.Errorf("provider body missing resolved data uri\n%s", b)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// A chunk the SDK can decode, then the terminator — `data: [DONE]`
		// alone ends the stream without yielding an event, which the provider
		// now reports. The assertion above is on the request body.
		_, _ = w.Write([]byte(
			`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	// Blob store: put the image, get the handle.
	store, err := media.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(strings.NewReader("hello"), "image/png", media.Policy{Allow: []string{"image/*"}})
	if err != nil {
		t.Fatal(err)
	}

	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	gw := &schemaGW{}
	a := session.New("main|test",
		session.Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{"llm.chat": true}},
		&session.Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil,
		map[string]session.OpHandler{
			"llm.chat": LLMHandlers(mgr, store)["llm.chat"],
		},
		pool.New(1), slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.LoopSrc = `
function loop()
  local msg = session.inbox()
  local parts = {}
  for _, att in ipairs(msg.attachments or {}) do
    table.insert(parts, { type = "text", text = "describe" })
    table.insert(parts, att)
  end
  local info = agent.info()
  local ok, reply = pcall(llm.chat, { { role = "user", parts = parts } }, { model = info.model })
  if ok then session.send("done") else session.send("err: " .. tostring(reply)) end
end
`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	// Deliver a message carrying the media handle as an attachment.
	a.Mailbox <- session.Message{
		ID: "m1", Type: "user", From: "u", Text: "describe this",
		Channel: "telegram", ReplyTo: "123",
		Attachments: []media.Part{{Type: "image", MIME: "image/png", Handle: ref.Handle, Name: "pic.png"}},
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gw.mu.Lock()
		n := len(gw.sends)
		gw.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	gw.mu.Lock()
	sends := append([]string(nil), gw.sends...)
	gw.mu.Unlock()
	if len(sends) == 0 {
		t.Fatal("no reply from loop")
	}
	if !strings.HasPrefix(sends[0], "done") {
		t.Fatalf("loop failed: %s", sends[0])
	}
	// The provider was hit and its assertion passed (or the test already
	// failed above).
}
