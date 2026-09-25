package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"agentflow/internal/config"

	"google.golang.org/genai"
	"google.golang.org/genai/interactions/models/apierrors"
	"google.golang.org/genai/interactions/models/interactions"
	"google.golang.org/genai/interactions/models/operations"
	"google.golang.org/genai/interactions/retry"
)

// defaultGoogleSearchURL is the Gemini Interactions API endpoint; the
// google_search engine is Gemini's Google Search grounding tool, not a
// plain web index — the model searches and synthesizes, citations come back
// as url_citation annotations on the generated text.
const defaultGoogleSearchURL = "https://generativelanguage.googleapis.com/v1beta/interactions"

// defaultGoogleSearchVersion is the version segment of defaultGoogleSearchURL:
// what the SDK is told to address when a configured base_url carries none of its
// own (see splitGoogleSearchBase).
const defaultGoogleSearchVersion = "v1beta"

// defaultGoogleSearchModel is the grounding-capable model this engine uses
// unless config overrides it (SearchEngine.Model).
const defaultGoogleSearchModel = "gemini-3.8-flash"

// googleSearch is a Searcher backed by Gemini's Google Search grounding.
// Unlike index engines (doubao/ollama) the response is a synthesized answer
// plus citation annotations, so the first WebResult carries the synthesis in
// Content and the rest are the cited sources.
//
// The wire is the official SDK's (google.golang.org/genai): base_url maps onto
// ClientConfig's BaseURL/APIVersion, the request is a typed
// CreateModelInteraction carrying the google_search Tool, and the reply is
// decoded into the SDK's Interaction — which models exactly what this engine
// reads: model_output text content, and url_citation annotations on it.
type googleSearch struct {
	apiKey string
	base   string // as configured: the full {prefix}/interactions endpoint
	model  string
	http   *http.Client
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
	return &googleSearch{apiKey: cfg.APIKey, base: base, model: model, http: &http.Client{Timeout: cfg.TimeoutD()}}, nil
}

// splitGoogleSearchBase recovers the SDK's two URL knobs from the endpoint this
// engine's base_url names. The SDK composes {BaseURL}/{APIVersion}/interactions,
// and base_url is documented (and defaulted) as the whole
// {prefix}/version/interactions endpoint, so the endpoint suffix comes off, then
// the last remaining path segment becomes the API version — which reproduces the
// configured URL exactly. A base with no path of its own (an httptest address,
// say) has no segment to lift, so the version this engine targets is used and
// the URL gains the /v1beta the endpoint template requires.
func splitGoogleSearchBase(base string) (baseURL, apiVersion string) {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(base, "/"), "/interactions")
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return trimmed, defaultGoogleSearchVersion
	}
	path := strings.Trim(u.Path, "/")
	if path == "" {
		return trimmed, defaultGoogleSearchVersion
	}
	origin := u.Scheme + "://" + u.Host
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return origin + "/" + path[:i], path[i+1:]
	}
	return origin, path
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

	baseURL, apiVersion := splitGoogleSearchBase(g.base)
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:     g.apiKey,
		Backend:    genai.BackendGeminiAPI,
		HTTPClient: g.http,
		HTTPOptions: genai.HTTPOptions{
			BaseURL:    baseURL,
			APIVersion: apiVersion,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("search google_search: %w", err)
	}

	input := interactions.NewInteractionsInput(query)
	body := operations.NewCreateInteractionRequestBody(interactions.CreateModelInteraction{
		Model: interactions.Model(g.model),
		Input: &input,
		Tools: []interactions.Tool{interactions.NewTool(interactions.GoogleSearch{})},
	})
	// One attempt: the engine reports a failure to its caller rather than
	// sitting on the SDK's default four-attempt backoff.
	resp, err := client.Interactions.Create(ctx, operations.CreateInteractionRequest{
		APIVersion: &apiVersion,
		Body:       body,
	}, operations.WithRetries(retry.Config{Strategy: "none"}))
	if err != nil {
		return nil, fmt.Errorf("search google_search: %s", googleSearchError(err))
	}
	if resp.Interaction == nil {
		return nil, fmt.Errorf("search google_search: empty response (no answer, no citations)")
	}

	var answer strings.Builder
	seen := map[string]bool{}
	citations := []WebResult{}
	for _, step := range resp.Interaction.Steps {
		// Only model_output steps carry the generated text; the search-call
		// steps hold the issued queries and the other step types are results of
		// other tools.
		if step.ModelOutputStep == nil {
			continue
		}
		for _, c := range step.ModelOutputStep.Content {
			if c.TextContent == nil {
				continue
			}
			answer.WriteString(c.TextContent.Text)
			for _, a := range c.TextContent.Annotations {
				if a.URLCitation == nil || a.URLCitation.URL == nil || *a.URLCitation.URL == "" || seen[*a.URLCitation.URL] {
					continue
				}
				seen[*a.URLCitation.URL] = true
				title := *a.URLCitation.URL
				if a.URLCitation.Title != nil && *a.URLCitation.Title != "" {
					title = *a.URLCitation.Title
				}
				citations = append(citations, WebResult{Title: title, URL: *a.URLCitation.URL, Site: hostOf(*a.URLCitation.URL)})
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

// googleSearchError renders an SDK failure in the shape this engine has always
// reported: the HTTP status, then the API's own message when the response
// carried one, then the body truncated. An error with no status never reached
// the provider, so the transport's own text is the whole story.
func googleSearchError(err error) string {
	code := 0
	body := err.Error()
	var apiErr *apierrors.APIError
	if errors.As(err, &apiErr) {
		code, body = apiErr.StatusCode, apiErr.Body
	}
	var clientErr *apierrors.CreateInteractionClientError
	if errors.As(err, &clientErr) && clientErr.HTTPMeta.Response != nil {
		code = clientErr.HTTPMeta.Response.StatusCode
	}
	var serverErr *apierrors.CreateInteractionServerError
	if errors.As(err, &serverErr) && serverErr.HTTPMeta.Response != nil {
		code = serverErr.HTTPMeta.Response.StatusCode
	}
	if code == 0 {
		return body
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &e) == nil && e.Error.Message != "" {
		return fmt.Sprintf("status %d: %s", code, e.Error.Message)
	}
	return fmt.Sprintf("status %d: %s", code, truncate([]byte(body), 512))
}

// hostOf extracts the bare host for WebResult.Site; "" when unparseable.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}
