package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"agentflow/internal/config"
)

// RankResult is one reranked document: Index is the 0-based position in the
// request's documents list, Score is the model's relevance score.
type RankResult struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

// Rerank scores documents against a query with a cross-encoder model and
// returns the results in the server's ranked order. The model must use
// provider "rerank", which speaks the de-facto standard POST
// {base_url}/rerank shape shared by Jina, Cohere v2, TEI, and vLLM.
func (m *Manager) Rerank(ctx context.Context, model, query string, docs []string, topN int) ([]RankResult, error) {
	cfg, err := m.resolve(model)
	if err != nil {
		return nil, err
	}
	if query == "" {
		return nil, fmt.Errorf("rerank: query is required")
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("rerank: at least one document is required")
	}
	var results []RankResult
	err = m.doWithRetry(ctx, cfg, "rerank", func(ctx context.Context) (bool, error) {
		r, retryable, err := openRerank(ctx, m.http, cfg, query, docs, topN)
		if err != nil {
			return retryable, err
		}
		results = r
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// openRerank routes a rerank request to the provider implementation.
func openRerank(ctx context.Context, client *http.Client, cfg config.Model, query string, docs []string, topN int) ([]RankResult, bool, error) {
	switch cfg.Provider {
	case "rerank":
		return rerankHTTP(ctx, client, cfg, query, docs, topN)
	}
	return nil, false, fmt.Errorf("provider %q does not support reranking; use provider \"rerank\" pointed at a Jina/Cohere/TEI/vLLM-compatible endpoint", cfg.Provider)
}

// rerankHTTP implements POST {base_url}/rerank:
//
//	request:  {"model","query","documents",[,"top_n"]}
//	response: {"results":[{"index","relevance_score"|"score"},...]}
//
// TEI's native server returns a bare array instead of the wrapped object;
// both shapes parse. base_url is required (there is no official host shared
// across compatible servers).
func rerankHTTP(ctx context.Context, client *http.Client, cfg config.Model, query string, docs []string, topN int) ([]RankResult, bool, error) {
	if cfg.BaseURL == "" {
		return nil, false, fmt.Errorf("provider \"rerank\": base_url is required (e.g. http://127.0.0.1:8080 for a local TEI/vLLM server)")
	}
	url := resolveBase(cfg, "") + "/rerank"

	body := map[string]any{
		"model":     cfg.Model,
		"query":     query,
		"documents": docs,
	}
	if topN > 0 {
		body["top_n"] = topN
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(mustJSON(body)))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("content-type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		retryable, serr := statusError(resp.StatusCode, b)
		return nil, retryable, fmt.Errorf("%s -> %w", url, serr)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, false, fmt.Errorf("%s -> read rerank response: %w", url, err)
	}
	results, err := parseRerankResults(raw)
	if err != nil {
		return nil, false, fmt.Errorf("%s -> %w", url, err)
	}
	return results, false, nil
}

// parseRerankResults tolerates the two wire shapes in the wild: a wrapped
// {"results":[...]} object (Jina/Cohere/vLLM) and a bare [...] array (TEI).
// Scores arrive as "relevance_score" or "score" depending on the server.
func parseRerankResults(raw []byte) ([]RankResult, error) {
	type entry struct {
		Index          int      `json:"index"`
		RelevanceScore *float64 `json:"relevance_score"`
		Score          *float64 `json:"score"`
	}
	var entries []entry
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, fmt.Errorf("decode rerank response: %w", err)
		}
	} else {
		var wrapped struct {
			Results []entry `json:"results"`
		}
		if err := json.Unmarshal(trimmed, &wrapped); err != nil {
			return nil, fmt.Errorf("decode rerank response: %w", err)
		}
		entries = wrapped.Results
	}
	out := make([]RankResult, 0, len(entries))
	for _, e := range entries {
		score := 0.0
		if e.RelevanceScore != nil {
			score = *e.RelevanceScore
		} else if e.Score != nil {
			score = *e.Score
		}
		out = append(out, RankResult{Index: e.Index, Score: score})
	}
	return out, nil
}
