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

// defaultXSearchURL is the xAI Responses API endpoint; the x_search engine is
// Grok's agentic X (Twitter) search tool — the model searches X and
// synthesizes, citations come back as top-level response citations.
const defaultXSearchURL = "https://api.x.ai/v1/responses"

// defaultXSearchModel is the tool-capable model this engine uses unless
// config overrides it (SearchEngine.Model).
const defaultXSearchModel = "grok-4.6"

// xSearch is a Searcher backed by xAI's x_search tool. Like google_search it
// returns a synthesis plus citations rather than raw index hits, so the first
// WebResult carries the answer in Content and the rest are cited sources
// (mostly x.com post URLs).
type xSearch struct {
	apiKey  string
	baseURL string
	model   string
	http    *http.Client
}

func newXSearch(cfg config.SearchEngine) (*xSearch, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("search x_search: api_key is required (set XAI_API_KEY)")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultXSearchURL
	}
	model := cfg.Model
	if model == "" {
		model = defaultXSearchModel
	}
	return &xSearch{apiKey: cfg.APIKey, baseURL: base, model: model, http: &http.Client{Timeout: cfg.TimeoutD()}}, nil
}

type xSearchRequest struct {
	Model string `json:"model"`
	Input []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"input"`
	Tools []struct {
		Type string `json:"type"`
	} `json:"tools"`
}

// xSearchResponse mirrors the Responses API: output_text is the convenience
// field, output[] carries message items with output_text content blocks, and
// citations is a top-level array. Citations have appeared both as bare URL
// strings and as {url} objects, so they decode as json.RawMessage first.
type xSearchResponse struct {
	OutputText string `json:"output_text"`
	Output     []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Citations []json.RawMessage `json:"citations"`
}

// citationURL decodes one citation entry, accepting both "https://…" and
// {"url": "https://…"} shapes.
func citationURL(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var o struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &o); err == nil {
		return o.URL
	}
	return ""
}

// Search runs one X-search query through the xAI Responses API.
func (x *xSearch) Search(ctx context.Context, req Request) (*Result, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, fmt.Errorf("search x_search: query is required")
	}
	count := req.Count
	if count <= 0 {
		count = 10
	}

	body := xSearchRequest{Model: x.model}
	body.Input = append(body.Input, struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{Role: "user", Content: query})
	body.Tools = append(body.Tools, struct {
		Type string `json:"type"`
	}{Type: "x_search"})
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("search x_search: marshal request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, x.baseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("search x_search: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+x.apiKey)

	resp, err := x.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("search x_search: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("search x_search: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := truncate(payload, 512)
		return nil, fmt.Errorf("search x_search: status %d: %s", resp.StatusCode, msg)
	}

	var xr xSearchResponse
	if err := json.Unmarshal(payload, &xr); err != nil {
		return nil, fmt.Errorf("search x_search: decode response: %w", err)
	}

	answer := strings.TrimSpace(xr.OutputText)
	if answer == "" {
		var b strings.Builder
		for _, item := range xr.Output {
			if item.Type != "" && item.Type != "message" {
				continue
			}
			for _, c := range item.Content {
				if c.Type == "" || c.Type == "output_text" || c.Type == "text" {
					b.WriteString(c.Text)
				}
			}
		}
		answer = strings.TrimSpace(b.String())
	}

	seen := map[string]bool{}
	citations := []WebResult{}
	for _, raw := range xr.Citations {
		u := citationURL(raw)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		citations = append(citations, WebResult{Title: u, URL: u, Site: hostOf(u)})
	}

	out := &Result{Query: query, Results: make([]WebResult, 0, len(citations)+1)}
	if answer != "" {
		out.Results = append(out.Results, WebResult{
			Title:   "Synthesis (" + x.model + " + X search)",
			Snippet: truncate([]byte(answer), 280),
			Content: answer,
		})
	}
	if len(citations) > count {
		citations = citations[:count]
	}
	out.Results = append(out.Results, citations...)
	if len(out.Results) == 0 {
		return nil, fmt.Errorf("search x_search: empty response (no answer, no citations)")
	}
	return out, nil
}
