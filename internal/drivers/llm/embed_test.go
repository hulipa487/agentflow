package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/media"
)

func embedManager(srv *httptest.Server, provider string) *Manager {
	return NewManager(map[string]config.Model{
		"default": {Provider: provider, Model: "embed-m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestEmbedRequestShapeAndParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["model"] != "embed-m" {
			t.Errorf("model = %v", body["model"])
		}
		inputs, ok := body["input"].([]any)
		if !ok || len(inputs) != 2 {
			t.Errorf("input = %v", body["input"])
		}
		// Return entries out of order; the driver must sort by index.
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"index": 1, "embedding": [0.2, 0.3]},
				{"index": 0, "embedding": [0.1, 0.4]}
			],
			"usage": {"prompt_tokens": 7, "total_tokens": 7}
		}`))
	}))
	defer srv.Close()

	vectors, usage, err := embedManager(srv, "openai").Embed(context.Background(), "default", []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 2 || vectors[0][0] != 0.1 || vectors[1][0] != 0.2 {
		t.Fatalf("vectors not ordered by index: %v", vectors)
	}
	if usage.Input != 7 {
		t.Fatalf("usage.Input = %d, want 7", usage.Input)
	}
}

func TestEmbedAnthropicUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, _, err := embedManager(srv, "anthropic").Embed(context.Background(), "default", []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "no embeddings API") {
		t.Fatalf("want explicit unsupported error, got %v", err)
	}
}

func TestEmbedRetriesOnServerError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}],"usage":{"prompt_tokens":1}}`))
	}))
	defer srv.Close()

	m := NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "embed-m", BaseURL: srv.URL, Retry: 1},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	vectors, _, err := m.Embed(context.Background(), "default", []string{"a"})
	if err != nil {
		t.Fatalf("Embed after retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	if len(vectors) != 1 || len(vectors[0]) != 1 {
		t.Fatalf("vectors = %v", vectors)
	}
}

func TestEmbedRequiresInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, _, err := embedManager(srv, "openai").Embed(context.Background(), "default", nil)
	if err == nil || !strings.Contains(err.Error(), "at least one input") {
		t.Fatalf("want input validation error, got %v", err)
	}
}

// embedOnceParts runs EmbedParts against a recording server.
func embedOnceParts(t *testing.T, parts []media.Part, eo EmbedOpts) ([][]float32, string) {
	t.Helper()
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":7}}`))
	}))
	defer srv.Close()
	m := NewManager(map[string]config.Model{
		"omni": {Provider: "openai", Model: "jina-embeddings-v5-omni-small", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	vecs, _, err := m.EmbedParts(context.Background(), "omni", parts, eo)
	if err != nil {
		t.Fatalf("EmbedParts: %v", err)
	}
	return vecs, body
}

func TestEmbedTextOnlyStaysPlainStrings(t *testing.T) {
	_, body := embedOnceParts(t, []media.Part{
		{Type: "text", Text: "a"},
		{Type: "text", Text: "b"},
	}, EmbedOpts{})
	// No doc objects, no task/dimensions: vanilla OpenAI shape.
	for _, bad := range []string{`"text":"`, `"image"`, `"task"`, `"dimensions"`} {
		if strings.Contains(body, bad) {
			t.Errorf("text-only body should stay plain strings, contains %q\n%s", bad, body)
		}
	}
	if !strings.Contains(body, `"input":["a","b"]`) {
		t.Errorf("body missing plain string input\n%s", body)
	}
}

func TestEmbedImageDocRawBase64AndURL(t *testing.T) {
	_, body := embedOnceParts(t, []media.Part{
		{Type: "image", MIME: "image/png", Data: "aGVsbG8="},
		{Type: "image", URL: "https://x.test/cat.jpg"},
	}, EmbedOpts{Task: "retrieval.query", Dimensions: 512})
	for _, want := range []string{
		`"image":"aGVsbG8="`, // raw base64, no data: prefix
		`"image":"https://x.test/cat.jpg"`,
		`"task":"retrieval.query"`,
		`"dimensions":512`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestEmbedMergedContentGroup(t *testing.T) {
	_, body := embedOnceParts(t, []media.Part{
		{Type: "text", Text: "a cat photo"},
		{Type: "image", Data: "aGVsbG8="},
	}, EmbedOpts{Merged: true})
	for _, want := range []string{`"content":[{"text":"a cat photo"},{"image":"aGVsbG8="}]`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestEmbedAudioVideoDocs(t *testing.T) {
	_, body := embedOnceParts(t, []media.Part{
		{Type: "audio", Data: "QUJD"},
		{Type: "video", URL: "https://x.test/v.mp4"},
	}, EmbedOpts{})
	for _, want := range []string{`"audio":"QUJD"`, `"video":"https://x.test/v.mp4"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestEmbedPDFSingleOnly(t *testing.T) {
	// pdf alone is fine
	_, body := embedOnceParts(t, []media.Part{
		{Type: "file", MIME: "application/pdf", Data: "UERG"},
	}, EmbedOpts{})
	if !strings.Contains(body, `"pdf":"UERG"`) {
		t.Errorf("body missing pdf doc\n%s", body)
	}
	// pdf in a batch errors before any request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request should not be sent for invalid pdf batch")
	}))
	defer srv.Close()
	m := NewManager(map[string]config.Model{
		"omni": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, _, err := m.EmbedParts(context.Background(), "omni", []media.Part{
		{Type: "file", MIME: "application/pdf", Data: "UERG"},
		{Type: "text", Text: "x"},
	}, EmbedOpts{}); err == nil || !strings.Contains(err.Error(), "single-item") {
		t.Fatalf("want single-item pdf error, got %v", err)
	}
}

func TestEmbedUnsupportedFileMime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request should not be sent")
	}))
	defer srv.Close()
	m := NewManager(map[string]config.Model{
		"omni": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, _, err := m.EmbedParts(context.Background(), "omni", []media.Part{
		{Type: "file", MIME: "application/zip", Data: "eHg="},
	}, EmbedOpts{}); err == nil || !strings.Contains(err.Error(), "no embeddings doc form") {
		t.Fatalf("want no-doc-form error, got %v", err)
	}
}
