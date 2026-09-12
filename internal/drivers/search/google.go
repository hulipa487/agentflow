package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"agentflow/internal/config"
)

// defaultGoogleSearchURL is the Gemini Interactions API endpoint; the
// google_search engine is Gemini's Google Search grounding tool, not a
// plain web index — the model searches and synthesizes, citations come back
// as url_citation annotations on the generated text.
const defaultGoogleSearchURL = "https://generativelanguage.googleapis.com/v1beta/interactions"

// defaultGoogleSearchModel is the grounding-capable model this engine uses
// unless config overrides it (SearchEngine.Model).
const defaultGoogleSearchModel = "gemini-3.8-flash"

// googleSearch is a Searcher backed by Gemini's Google Search grounding.
// Unlike index engines (doubao/ollama) the response is a synthesized answer
// plus citation annotations, so the first WebResult carries the synthesis in
// Content and the rest are the cited sources.
type googleSearch struct {
	apiKey  string
	baseURL string
	model   string
	http    *http.Client
}

func newGoogleSearch(cfg config.SearchEngine) (*googleSearch, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("search google_search: api_key is required (set GEMINI_API_KEY)")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultGoogleSearchURL
	}
	model := cfg.Model
	if model == "" {
		model = defaultGoogleSearchModel
	}
	return &googleSearch{apiKey: cfg.APIKey, baseURL: base, model: model, http: &http.Client{Timeout: cfg.TimeoutD()}}, nil
}

type googleSearchRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
	Tools []struct {
		Type string `json:"type"`
	} `json:"tools"`
}

// googleSearchResponse mirrors the Interactions API shape: a steps array
// where google_search_call steps carry the issued queries and model_output
// steps carry content blocks; text blocks hold the answer and its
// url_citation annotations.
type googleSearchResponse struct {
	Steps []struct {
		Type    string `json:"type"`
		Content []struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Annotations []struct {
				Type  string `json:"type"`
				URL   string `json:"url"`
				Title string `json:"title"`
			} `json:"annotations"`
		} `json:"content"`
	} `json:"steps"`
}

// Search runs one grounded query through the Gemini Interactions API.
func (g *googleSearch) Search(ctx context.Context, req Request) (*Result, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, fmt.Errorf("search google_search: query is required")
	}
	count := req.Count
	if count <= 0 {
		count = 10
	}

	body := googleSearchRequest{Model: g.model, Input: query}
	body.Tools = append(body.Tools, struct {
		Type string `json:"type"`
	}{Type: "google_search"})
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("search google_search: marshal request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("search google_search: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("search google_search: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("search google_search: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(payload, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = truncate(payload, 512)
		}
		return nil, fmt.Errorf("search google_search: status %d: %s", resp.StatusCode, msg)
	}

	var gr googleSearchResponse
	if err := json.Unmarshal(payload, &gr); err != nil {
		return nil, fmt.Errorf("search google_search: decode response: %w", err)
	}

	var answer strings.Builder
	seen := map[string]bool{}
	citations := []WebResult{}
	for _, step := range gr.Steps {
		if step.Type != "" && step.Type != "model_output" {
			continue
		}
		for _, c := range step.Content {
			if c.Type != "" && c.Type != "text" {
				continue
			}
			answer.WriteString(c.Text)
			for _, a := range c.Annotations {
				if a.URL == "" || (a.Type != "" && a.Type != "url_citation") || seen[a.URL] {
					continue
				}
				seen[a.URL] = true
				title := a.Title
				if title == "" {
					title = a.URL
				}
				citations = append(citations, WebResult{Title: title, URL: a.URL, Site: hostOf(a.URL)})
			}
		}
	}

	out := &Result{Query: query, Results: make([]WebResult, 0, len(citations)+1)}
	if text := strings.TrimSpace(answer.String()); text != "" {
		out.Results = append(out.Results, WebResult{
			Title:   "Synthesis (" + g.model + " + Google Search)",
			Snippet: truncate([]byte(text), 280),
			Content: text,
		})
	}
	if len(citations) > count {
		citations = citations[:count]
	}
	out.Results = append(out.Results, citations...)
	if len(out.Results) == 0 {
		return nil, fmt.Errorf("search google_search: empty response (no answer, no citations)")
	}
	return out, nil
}

// hostOf extracts the bare host for WebResult.Site; "" when unparseable.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}
