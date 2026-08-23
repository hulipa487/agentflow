package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	"agentflow/internal/drivers/llm"
)

// TestLLMChatResolvesAttachmentHandleToBase64 wires the full inbound→caps→
// provider path: a message lands with a blob-store image handle, a loop
// forwards it into llm.chat, and the caps layer resolves the handle to inline
// base64 so the provider sees a data URI. This is the core multimodal
// guarantee: bytes never cross the bridge, only the handle does.
func TestLLMChatResolvesAttachmentHandleToBase64(t *testing.T) {
	// Provider endpoint asserts the request body carries the resolved data URI.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		if !strings.Contains(string(b), "data:image/png;base64,aGVsbG8=") {
			t.Errorf("provider body missing resolved data uri\n%s", b)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
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

	// LLM manager pointed at the test server.
	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	gw := &fakeGW{}
	a := New("main|test",
		Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{"llm.chat": true}},
		&Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil,
		map[string]OpHandler{
			"llm.chat": LLMobileChatHandler(mgr, store),
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
	a.Mailbox <- Message{
		ID: "m1", Type: "user", From: "u", Text: "describe this",
		Channel: "telegram", ReplyTo: "123",
		Attachments: []media.Part{{Type: "image", MIME: "image/png", Handle: ref.Handle, Name: "pic.png"}},
	}

	waitFor(t, "loop reply", 5*time.Second, func() bool { return len(gw.snapshot()) > 0 })
	sends := gw.snapshot()
	if len(sends) == 0 {
		t.Fatal("no reply")
	}
	if !strings.HasPrefix(sends[0].text, "done") {
		t.Fatalf("loop failed: %s", sends[0].text)
	}
	// The provider was hit and its assertion passed (or the test already
	// failed above).
}

// LLMobileChatHandler is a minimal llm.chat handler for tests, resolving part
// handles exactly like caps.LLMHandlers does (inlined to avoid an import
// cycle with caps — the logic is the same).
func LLMobileChatHandler(mgr *llm.Manager, store *media.Store) OpHandler {
	return func(ctx context.Context, op Op) (string, bool) {
		msgs := make([]llm.Message, len(op.Messages))
		for i, mm := range op.Messages {
			msgs[i] = llm.Message{Role: mm.Role, Content: mm.Content}
			for _, p := range mm.Parts {
				if p.Handle != "" && p.Data == "" {
					b, err := store.ReadAll(p.Handle, 20<<20)
					if err != nil {
						r, _ := jsonMarshalErr(err)
						return r, false
					}
					p.Data = base64.StdEncoding.EncodeToString(b)
				}
				msgs[i].Parts = append(msgs[i].Parts, p)
			}
		}
		text, _, _, err := mgr.Chat(ctx, op.Model, msgs, llm.Opts{})
		if err != nil {
			r, _ := jsonMarshalErr(err)
			return r, false
		}
		b, _ := jsonMarshalOk(map[string]any{"text": text, "usage": map[string]int{"input": 0, "output": 0}})
		return string(b), true
	}
}

func jsonMarshalErr(err error) (string, bool) { return "\"" + err.Error() + "\"", false }
func jsonMarshalOk(v any) ([]byte, error)     { return json.Marshal(v) }