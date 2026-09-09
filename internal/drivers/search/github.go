package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/config"
)

// The GitHub repository search API (api.github.com/search/repositories). No
// key is required: the anonymous search quota is 10 requests/minute per IP,
// and a token (api_key — a PAT or github token, repo scope not needed for
// public search) lifts it to 30/minute. GitHub rejects requests without a
// User-Agent, and rate-limits with 403 + Retry-After / X-RateLimit-Reset; the
// engine surfaces those honestly rather than hanging on a retry. The query
// takes GitHub's full qualifier syntax (language:go stars:>100 …); TimeRange
// maps onto a `pushed:` qualifier.
const (
	defaultGitHubURL = "https://api.github.com"
	maxGitHubCount   = 100 // per_page ceiling
	githubAPIVersion = "2022-11-28"
)

// github is a Searcher over /search/repositories. Each hit is a repository:
// URL is the repo page, Site is the top topics, Snippet is a popularity-signal
// prefix (stars · language · forks) plus the description, Content is the
// description, and Score is the raw stargazer count (engine-native, not 0..1 —
// GitHub's own relevance score is ~1.0 for every repo and carries no signal).
type github struct {
	baseURL string
	key     string
	http    *http.Client
}

func newGitHub(cfg config.SearchEngine) (*github, error) {
	base := cfg.BaseURL
	if base == "" {
		base = defaultGitHubURL
	}
	return &github{
		baseURL: strings.TrimSuffix(base, "/"),
		key:     cfg.APIKey,
		http:    &http.Client{Timeout: cfg.TimeoutD()},
	}, nil
}

type githubResponse struct {
	TotalCount       int  `json:"total_count"`
	IncompleteResult bool `json:"incomplete_results"`
	Items            []struct {
		FullName    string   `json:"full_name"`
		HTMLURL     string   `json:"html_url"`
		Description string   `json:"description"`
		Language    string   `json:"language"`
		Stars       int      `json:"stargazers_count"`
		Forks       int      `json:"forks_count"`
		Topics      []string `json:"topics"`
		PushedAt    string   `json:"pushed_at"`
	} `json:"items"`
	// Error envelope (present on failures, with a non-200 status).
	Message string `json:"message"`
}

// Search runs one repository search.
func (g *github) Search(ctx context.Context, req Request) (*Result, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, fmt.Errorf("search github: query is required")
	}
	count := req.Count
	if count <= 0 {
		count = 10
	}
	if count > maxGitHubCount {
		count = maxGitHubCount
	}
	if q := githubTimeRange(req.TimeRange); q != "" {
		query += " pushed:" + q
	}

	q := url.Values{}
	q.Set("q", query)
	q.Set("per_page", strconv.Itoa(count))
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+"/search/repositories?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("search github: build request: %w", err)
	}
	hreq.Header.Set("Accept", "application/vnd.github+json")
	hreq.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	hreq.Header.Set("User-Agent", "agentflow") // GitHub 403s requests with no UA
	if g.key != "" {
		hreq.Header.Set("Authorization", "Bearer "+g.key)
	}

	resp, err := g.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("search github: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("search github: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &e)
		msg := e.Message
		if msg == "" {
			msg = truncate(payload, 512)
		}
		// Rate-limited (anonymous 10/min, or an abuse/secondary limit): say when
		// to retry instead of leaving the caller guessing.
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				msg += " (retry after " + ra + "s)"
			} else if resp.Header.Get("X-RateLimit-Remaining") == "0" {
				msg += " (search rate limit; an api_key lifts 10/min to 30/min)"
			}
		}
		return nil, fmt.Errorf("search github: status %d: %s", resp.StatusCode, msg)
	}

	var gr githubResponse
	if err := json.Unmarshal(payload, &gr); err != nil {
		return nil, fmt.Errorf("search github: decode response: %w", err)
	}
	out := &Result{Query: strings.TrimSpace(req.Query), Results: make([]WebResult, 0, len(gr.Items))}
	for _, it := range gr.Items {
		w := WebResult{
			Title:     it.FullName,
			URL:       it.HTMLURL,
			Content:   it.Description,
			Score:     float64(it.Stars),
			Published: it.PushedAt, // last code push, RFC3339
		}
		if len(it.Topics) > 0 {
			topics := it.Topics
			if len(topics) > 3 {
				topics = topics[:3]
			}
			w.Site = strings.Join(topics, ", ")
		}
		signal := fmt.Sprintf("★ %d", it.Stars)
		if it.Language != "" {
			signal += " · " + it.Language
		}
		signal += fmt.Sprintf(" · %d forks", it.Forks)
		if it.Description != "" {
			w.Snippet = signal + ": " + truncate([]byte(it.Description), 240-len(signal)-2)
		} else {
			w.Snippet = signal
		}
		out.Results = append(out.Results, w)
	}
	return out, nil
}

// githubTimeRange maps the shared TimeRange enum (and the YYYY-MM-DD..YYYY-MM-DD
// range form) onto a GitHub `pushed:` date qualifier value. GitHub accepts the
// range form natively; the enum maps to >=<date>.
func githubTimeRange(tr string) string {
	switch tr {
	case "OneDay":
		return ">=" + time.Now().Add(-24*time.Hour).Format("2006-01-02")
	case "OneWeek":
		return ">=" + time.Now().Add(-7*24*time.Hour).Format("2006-01-02")
	case "OneMonth":
		return ">=" + time.Now().Add(-30*24*time.Hour).Format("2006-01-02")
	case "OneYear":
		return ">=" + time.Now().Add(-365*24*time.Hour).Format("2006-01-02")
	}
	parts := strings.SplitN(tr, "..", 2)
	if len(parts) != 2 {
		return ""
	}
	if _, e1 := time.Parse("2006-01-02", parts[0]); e1 != nil {
		return ""
	}
	if _, e2 := time.Parse("2006-01-02", parts[1]); e2 != nil {
		return ""
	}
	return tr
}
