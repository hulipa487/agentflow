package caps

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/media"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/llm"
)

// embedMgrFor builds an llm.Manager with one "omni" openai-shaped model
// pointed at url.
func embedMgrFor(url string) *llm.Manager {
	return llm.NewManager(map[string]config.Model{
		"omni": {Provider: "openai", Model: "jina-embeddings-v5-omni-small", BaseURL: url},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestEmbedHandlerResolvesHandleAndPassesOpts drives the llm.embed caps
// handler with a mixed batch (text string part + blob-store image handle)
// and asserts the request body: handle resolved to raw base64, Jina doc
// objects, task/dimensions passthrough.
func TestEmbedHandlerResolvesHandleAndPassesOpts(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":3}}`))
	}))
	defer srv.Close()

	store, err := media.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(strings.NewReader("imgbytes"), "image/png", media.Policy{Allow: []string{"image/*"}})
	if err != nil {
		t.Fatal(err)
	}

	h := LLMHandlers(embedMgrFor(srv.URL), store)
	resp, ok := h["llm.embed"](context.Background(), session.Op{
		Type:  "llm.embed",
		Model: "omni",
		Inputs: []media.Part{
			{Type: "text", Text: "describe"},
			{Type: "image", MIME: "image/png", Handle: ref.Handle, Name: "p.png"},
		},
		Task:       "retrieval.passage",
		Dimensions: 256,
	})
	if !ok {
		t.Fatalf("llm.embed failed: %s", resp)
	}
	// imgbytes -> base64
	const b64 = "aW1nYnl0ZXM="
	for _, want := range []string{
		`"text":"describe"`,
		`"image":"` + b64 + `"`, // handle resolved to raw base64
		`"task":"retrieval.passage"`,
		`"dimensions":256`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

// TestEmbedHandlerUnresolvedHandleFails: no store wired -> clear error, no request.
func TestEmbedHandlerUnresolvedHandleFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be sent")
	}))
	defer srv.Close()
	h := LLMHandlers(embedMgrFor(srv.URL), nil)
	resp, ok := h["llm.embed"](context.Background(), session.Op{
		Type:   "llm.embed",
		Model:  "omni",
		Inputs: []media.Part{{Type: "image", MIME: "image/png", Handle: "media:" + strings.Repeat("a", 64)}},
	})
	if ok {
		t.Fatalf("expected failure, got %s", resp)
	}
	if !strings.Contains(resp, "no media store configured") {
		t.Fatalf("wrong error: %s", resp)
	}
}
