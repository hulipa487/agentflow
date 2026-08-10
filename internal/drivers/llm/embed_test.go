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
