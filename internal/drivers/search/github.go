package search

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agentflow/internal/config"

	ghapi "github.com/google/go-github/v66/github"
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
)

// github is a Searcher over /search/repositories. Each hit is a repository:
// URL is the repo page, Site is the top topics, Snippet is a popularity-signal
// prefix (stars · language · forks) plus the description, Content is the
// description, and Score is the raw stargazer count (engine-native, not 0..1 —
// GitHub's own relevance score is ~1.0 for every repo and carries no signal).
type github struct {
	client *ghapi.Client
}

func newGitHub(cfg config.SearchEngine) (*github, error) {
	base := cfg.BaseURL
	if base == "" {
		base = defaultGitHubURL
	}
	u, err := url.Parse(base + "/")
	if err != nil {
		return nil, fmt.Errorf("search github: invalid base_url: %w", err)
	}
	client := ghapi.NewClient(&http.Client{Timeout: cfg.TimeoutD()})
	client.BaseURL = u
	if cfg.APIKey != "" {
		client = client.WithAuthToken(cfg.APIKey)
	}
	return &github{client: client}, nil
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

	result, _, err := g.client.Search.Repositories(ctx, query, &ghapi.SearchOptions{ListOptions: ghapi.ListOptions{PerPage: count}})
	if err != nil {
		return nil, g.formatError(err)
	}

	out := &Result{Query: strings.TrimSpace(req.Query), Results: make([]WebResult, 0, len(result.Repositories))}
	for _, it := range result.Repositories {
		if it == nil {
			continue
		}
		stars := it.GetStargazersCount()
		forks := it.GetForksCount()
		desc := it.GetDescription()
		w := WebResult{
			Title:     it.GetFullName(),
			URL:       it.GetHTMLURL(),
			Content:   desc,
			Score:     float64(stars),
			Published: "",
		}
		if it.PushedAt != nil {
			w.Published = it.GetPushedAt().Format(time.RFC3339)
		}
		topics := it.Topics
		if len(topics) > 0 {
			if len(topics) > 3 {
				topics = topics[:3]
			}
			w.Site = strings.Join(topics, ", ")
		}
		signal := fmt.Sprintf("★ %d", stars)
		if lang := it.GetLanguage(); lang != "" {
			signal += " · " + lang
		}
		signal += fmt.Sprintf(" · %d forks", forks)
		if desc != "" {
			w.Snippet = signal + ": " + truncate([]byte(desc), 240-len(signal)-2)
		} else {
			w.Snippet = signal
		}
		out.Results = append(out.Results, w)
	}
	return out, nil
}

// formatError turns a go-github error into the same honest text the hand-
// rolled client produced, including rate-limit hints.
func (g *github) formatError(err error) error {
	msg := err.Error()
	status := 0
	var h http.Header

	switch e := err.(type) {
	case *ghapi.RateLimitError:
		status = e.Response.StatusCode
		h = e.Response.Header
	case *ghapi.AbuseRateLimitError:
		status = e.Response.StatusCode
		h = e.Response.Header
		if e.RetryAfter != nil {
			msg += fmt.Sprintf(" (retry after %ds)", int(e.RetryAfter.Seconds()))
			return fmt.Errorf("search github: status %d: %s", status, msg)
		}
	default:
		var er *ghapi.ErrorResponse
		if errors.As(err, &er) {
			status = er.Response.StatusCode
			h = er.Response.Header
			msg = er.Message
		}
	}

	if status != 0 && (status == http.StatusForbidden || status == http.StatusTooManyRequests) {
		if ra := h.Get("Retry-After"); ra != "" {
			msg += " (retry after " + ra + "s)"
		} else if h.Get("X-RateLimit-Remaining") == "0" {
			msg += " (search rate limit; an api_key lifts 10/min to 30/min)"
		}
	}
	if status != 0 {
		return fmt.Errorf("search github: status %d: %s", status, msg)
	}
	return fmt.Errorf("search github: %w", err)
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
