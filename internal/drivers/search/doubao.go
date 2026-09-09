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

// defaultDoubaoURL is the API-key-auth endpoint for the Doubao (Volcano
// Engine) web-search Custom API. The TOP-gateway (AK/SK) endpoint is not
// supported; API-key auth is the recommended integration.
const defaultDoubaoURL = "https://open.feedcoopapi.com/search_api/web_search"

// maxDoubaoCount caps a web search at the API's documented ceiling.
const maxDoubaoCount = 50

// doubao is a Searcher backed by the Doubao web-search Custom API. The API key
// rides in the Authorization header and is redacted from any error that could
// reach logs.
type doubao struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

func newDoubao(cfg config.SearchEngine) (*doubao, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("search doubao: api_key is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultDoubaoURL
	}
	return &doubao{apiKey: cfg.APIKey, baseURL: base, http: &http.Client{Timeout: cfg.TimeoutD()}}, nil
}

// --- wire shapes (the API uses PascalCase field names) ----------------------

type doubaoRequest struct {
	Query      string        `json:"Query"`
	SearchType string        `json:"SearchType"`
	Count      int           `json:"Count,omitempty"`
	TimeRange  string        `json:"TimeRange,omitempty"`
	Filter     *doubaoFilter `json:"Filter,omitempty"`
}

type doubaoFilter struct {
	NeedContent bool   `json:"NeedContent"`
	NeedURL     bool   `json:"NeedUrl"`
	Sites       string `json:"Sites,omitempty"`
	BlockHosts  string `json:"BlockHosts,omitempty"`
}

type doubaoResponse struct {
	ResponseMetadata struct {
		RequestID string `json:"RequestId"`
		Error     *struct {
			CodeN   int    `json:"CodeN"`
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
	} `json:"ResponseMetadata"`
	Result *struct {
		ResultCount int `json:"ResultCount"`
		WebResults  []struct {
			Title       string  `json:"Title"`
			SiteName    string  `json:"SiteName"`
			URL         string  `json:"Url"`
			Snippet     string  `json:"Snippet"`
			Summary     string  `json:"Summary"`
			Content     string  `json:"Content"`
			PublishTime string  `json:"PublishTime"`
			RankScore   float64 `json:"RankScore"`
		} `json:"WebResults"`
		SearchContext struct {
			OriginQuery string `json:"OriginQuery"`
			SearchType  string `json:"SearchType"`
		} `json:"SearchContext"`
		TimeCost int64 `json:"TimeCost"`
	} `json:"Result"`
}

// Search runs one web search against the Doubao API.
func (d *doubao) Search(ctx context.Context, req Request) (*Result, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, fmt.Errorf("search doubao: query is required")
	}
	count := req.Count
	if count <= 0 {
		count = 10
	}
	if count > maxDoubaoCount {
		count = maxDoubaoCount
	}
	body := doubaoRequest{
		Query:      query,
		SearchType: "web",
		Count:      count,
		TimeRange:  req.TimeRange,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("search doubao: marshal request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("search doubao: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+d.apiKey)

	resp, err := d.http.Do(hreq)
	if err != nil {
		return nil, d.redact(fmt.Errorf("search doubao: %w", err))
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("search doubao: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, d.redact(fmt.Errorf("search doubao: %s -> status %d: %s", d.baseURL, resp.StatusCode, truncate(payload, 512)))
	}

	var dr doubaoResponse
	if err := json.Unmarshal(payload, &dr); err != nil {
		return nil, fmt.Errorf("search doubao: decode response: %w", err)
	}
	if e := dr.ResponseMetadata.Error; e != nil && (e.CodeN != 0 || e.Code != "") {
		return nil, fmt.Errorf("search doubao: api error %s (%d): %s", e.Code, e.CodeN, e.Message)
	}
	if dr.Result == nil {
		return nil, fmt.Errorf("search doubao: empty result (request %s)", dr.ResponseMetadata.RequestID)
	}

	out := &Result{
		Query:      dr.Result.SearchContext.OriginQuery,
		TimeCostMs: dr.Result.TimeCost,
		Results:    make([]WebResult, 0, len(dr.Result.WebResults)),
	}
	if out.Query == "" {
		out.Query = query
	}
	for _, w := range dr.Result.WebResults {
		out.Results = append(out.Results, WebResult{
			Title:     w.Title,
			URL:       w.URL,
			Site:      w.SiteName,
			Summary:   w.Summary,
			Snippet:   w.Snippet,
			Content:   w.Content,
			Published: w.PublishTime,
			Score:     w.RankScore,
		})
	}
	return out, nil
}

// redact ensures the API key never appears in an error string bound for logs.
func (d *doubao) redact(err error) error {
	if d.apiKey == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), d.apiKey, "***"))
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
