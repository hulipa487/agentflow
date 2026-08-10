package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/config"
)

func rerankManager(srv *httptest.Server, provider, baseURL string) *Manager {
	return NewManager(map[string]config.Model{
		"default": {Provider: provider, Model: "rerank-m", BaseURL: baseURL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRerankWrappedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/rerank") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["query"] != "capital of france" {
			t.Errorf("query = %v", body["query"])
		}
		if docs, ok := body["documents"].([]any); !ok || len(docs) != 2 {
			t.Errorf("documents = %v", body["documents"])
		}
		if body["top_n"].(float64) != 1 {
			t.Errorf("top_n = %v", body["top_n"])
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"index":1,"relevance_score":0.9}]}`))
	}))
	defer srv.Close()

	results, err := rerankManager(srv, "rerank", srv.URL).Rerank(
		context.Background(), "default", "capital of france", []string{"doc a", "doc b"}, 1)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(results) != 1 || results[0].Index != 1 || results[0].Score != 0.9 {
		t.Fatalf("results = %+v", results)
	}
}

func TestRerankBareArrayResponse(t *testing.T) {
	// TEI's native server returns a bare array with "score".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`[{"index":0,"score":0.3},{"index":1,"score":0.8}]`))
	}))
	defer srv.Close()

	results, err := rerankManager(srv, "rerank", srv.URL).Rerank(
		context.Background(), "default", "q", []string{"a", "b"}, 0)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(results) != 2 || results[1].Index != 1 || results[1].Score != 0.8 {
		t.Fatalf("results = %+v", results)
	}
}

func TestRerankRejectsChatProviders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, err := rerankManager(srv, "openai", srv.URL).Rerank(context.Background(), "default", "q", []string{"a"}, 0)
	if err == nil || !strings.Contains(err.Error(), "does not support reranking") {
		t.Fatalf("want provider error, got %v", err)
	}
}

func TestRerankRequiresBaseURL(t *testing.T) {
	m := rerankManager(nil, "rerank", "")
	_, err := m.Rerank(context.Background(), "default", "q", []string{"a"}, 0)
	if err == nil || !strings.Contains(err.Error(), "base_url is required") {
		t.Fatalf("want base_url error, got %v", err)
	}
}

func TestRerankValidatesArgs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	m := rerankManager(srv, "rerank", srv.URL)
	if _, err := m.Rerank(context.Background(), "default", "", []string{"a"}, 0); err == nil {
		t.Fatal("want query validation error")
	}
	if _, err := m.Rerank(context.Background(), "default", "q", nil, 0); err == nil {
		t.Fatal("want documents validation error")
	}
}
