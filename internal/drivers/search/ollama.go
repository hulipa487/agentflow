package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agentflow/internal/config"
)

// defaultOllamaURL is Ollama's hosted web-search REST API (an API key from
// ollama.com/settings/keys; generous free tier for individuals).
const defaultOllamaURL = "https://ollama.com/api/web_search"

// maxOllamaCount is the API's documented per-query ceiling.
const maxOllamaCount = 10

// ollama is a Searcher backed by the Ollama web-search API. Each hit is
// {title, url, content} — content is the relevant excerpt, so it fills both
// the snippet (truncated) and content fields of the normalized result.
type ollama struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

func newOllama(cfg config.SearchEngine) (*ollama, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("search ollama: api_key is required (create one at ollama.com/settings/keys)")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultOllamaURL
	}
	return &ollama{apiKey: cfg.APIKey, baseURL: base, http: &http.Client{Timeout: cfg.TimeoutD()}}, nil
}

type ollamaRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results,omitempty"`
}

type ollamaResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// Search runs one web search against the Ollama API.
func (o *ollama) Search(ctx context.Context, req Request) (*Result, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, fmt.Errorf("search ollama: query is required")
	}
	count := req.Count
	if count <= 0 {
		count = 5
	}
	if count > maxOllamaCount {
		count = maxOllamaCount
	}

	raw, err := json.Marshal(ollamaRequest{Query: query, MaxResults: count})
	if err != nil {
		return nil, fmt.Errorf("search ollama: marshal request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("search ollama: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("search ollama: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("search ollama: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(payload, &e)
		msg := e.Error
		if msg == "" {
			msg = truncate(payload, 512)
		}
		return nil, fmt.Errorf("search ollama: status %d: %s", resp.StatusCode, msg)
	}

	var or ollamaResponse
	if err := json.Unmarshal(payload, &or); err != nil {
		return nil, fmt.Errorf("search ollama: decode response: %w", err)
	}
	out := &Result{Query: query, Results: make([]WebResult, 0, len(or.Results))}
	for _, r := range or.Results {
		out.Results = append(out.Results, WebResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: truncate([]byte(r.Content), 280),
			Content: r.Content,
		})
	}
	return out, nil
}
