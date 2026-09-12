package search

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/config"
)

// --- google_search -----------------------------------------------------------

// googleOK is a trimmed real-shape Interactions API response: one
// google_search_call step, then a model_output step whose text block carries
// url_citation annotations.
const googleOK = `{
  "steps": [
    {"type": "google_search_call", "queries": ["euro 2024 winner"]},
    {"type": "model_output", "content": [{
      "type": "text",
      "text": "Spain won Euro 2024, beating England 2-1 in the final.",
      "annotations": [
        {"type": "url_citation", "url": "https://example.com/final", "title": "Final report", "start_index": 0, "end_index": 20},
        {"type": "url_citation", "url": "https://example.com/final", "title": "Final report", "start_index": 21, "end_index": 40},
        {"type": "url_citation", "url": "https://uefa.com/euro2024", "title": "UEFA", "start_index": 0, "end_index": 10}
      ]
    }]}
  ]
}`

func TestGoogleSearch(t *testing.T) {
	var gotKey, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-goog-api-key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, googleOK)
	}))
	defer srv.Close()

	s, err := newGoogleSearch(config.SearchEngine{APIKey: "g123", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Search(context.Background(), Request{Query: "euro 2024 winner", Count: 5})
	if err != nil {
		t.Fatal(err)
	}
	if gotKey != "g123" {
		t.Fatalf("api-key header: %q", gotKey)
	}
	// Default model + the google_search tool must be on the wire.
	if !strings.Contains(gotBody, `"model":"gemini-3.8-flash"`) || !strings.Contains(gotBody, `"type":"google_search"`) {
		t.Fatalf("request body: %s", gotBody)
	}
	// Synthesis first, then deduped citations.
	if len(res.Results) != 3 {
		t.Fatalf("results: %+v", res.Results)
	}
	if !strings.Contains(res.Results[0].Content, "Spain won Euro 2024") {
		t.Fatalf("synthesis: %+v", res.Results[0])
	}
	if res.Results[1].URL != "https://example.com/final" || res.Results[1].Site != "example.com" {
		t.Fatalf("citation: %+v", res.Results[1])
	}
}

func TestGoogleSearchRequiresKey(t *testing.T) {
	if _, err := newGoogleSearch(config.SearchEngine{}); err == nil {
		t.Fatal("expected error without api_key")
	}
}

func TestGoogleSearchModelOverride(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		io.WriteString(w, googleOK)
	}))
	defer srv.Close()
	s, _ := newGoogleSearch(config.SearchEngine{APIKey: "k", BaseURL: srv.URL, Model: "gemini-x"})
	if _, err := s.Search(context.Background(), Request{Query: "q"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"model":"gemini-x"`) {
		t.Fatalf("request body: %s", gotBody)
	}
}

func TestGoogleSearchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error": {"message": "API key not valid"}}`)
	}))
	defer srv.Close()
	s, _ := newGoogleSearch(config.SearchEngine{APIKey: "bad", BaseURL: srv.URL})
	_, err := s.Search(context.Background(), Request{Query: "q"})
	if err == nil || !strings.Contains(err.Error(), "API key not valid") {
		t.Fatalf("err: %v", err)
	}
}

// --- x_search ----------------------------------------------------------------

// xSearchOK is a trimmed real-shape Responses API reply with top-level
// citations in both supported shapes (bare string and {url} object).
const xSearchOK = `{
  "output_text": "People on X are excited about the launch.",
  "output": [],
  "citations": [
    "https://x.com/user/status/1",
    {"url": "https://x.com/other/status/2"},
    "https://x.com/user/status/1"
  ]
}`

func TestXSearch(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, xSearchOK)
	}))
	defer srv.Close()

	s, err := newXSearch(config.SearchEngine{APIKey: "x123", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Search(context.Background(), Request{Query: "xai on x", Count: 5})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer x123" {
		t.Fatalf("auth header: %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"grok-4.6"`) || !strings.Contains(gotBody, `"type":"x_search"`) {
		t.Fatalf("request body: %s", gotBody)
	}
	// Synthesis + 2 deduped citations.
	if len(res.Results) != 3 {
		t.Fatalf("results: %+v", res.Results)
	}
	if !strings.Contains(res.Results[0].Content, "excited about the launch") {
		t.Fatalf("synthesis: %+v", res.Results[0])
	}
	if res.Results[1].URL != "https://x.com/user/status/1" || res.Results[2].Site != "x.com" {
		t.Fatalf("citations: %+v", res.Results[1:])
	}
}

// xSearchWalkOK has no output_text convenience field — the answer must be
// walked out of output[].content[].
const xSearchWalkOK = `{
  "output": [
    {"type": "x_search_call", "id": "xs_1"},
    {"type": "message", "content": [{"type": "output_text", "text": "Walked answer."}]}
  ],
  "citations": []
}`

func TestXSearchOutputWalk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, xSearchWalkOK)
	}))
	defer srv.Close()
	s, _ := newXSearch(config.SearchEngine{APIKey: "k", BaseURL: srv.URL})
	res, err := s.Search(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 || res.Results[0].Content != "Walked answer." {
		t.Fatalf("results: %+v", res.Results)
	}
}

func TestXSearchRequiresKey(t *testing.T) {
	if _, err := newXSearch(config.SearchEngine{}); err == nil {
		t.Fatal("expected error without api_key")
	}
}

func TestXSearchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error": "Incorrect API key provided."}`)
	}))
	defer srv.Close()
	s, _ := newXSearch(config.SearchEngine{APIKey: "bad", BaseURL: srv.URL})
	_, err := s.Search(context.Background(), Request{Query: "q"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err: %v", err)
	}
}

// --- registration ------------------------------------------------------------

func TestNewEngineGroundedEngines(t *testing.T) {
	for _, name := range []string{"google_search", "x_search"} {
		if _, err := NewEngine(name, config.SearchEngine{APIKey: "k"}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
