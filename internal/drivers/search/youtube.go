package search

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"agentflow/internal/config"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	ytapi "google.golang.org/api/youtube/v3"
)

// The YouTube Data API v3 (www.googleapis.com/youtube/v3/search) — Google's
// own index of videos, channels, and playlists. An API key is required (the
// key= query param; a free Google Cloud key works). Quota is the constraint:
// search.list costs 100 units per call against a default 10,000/day project
// quota, i.e. ~100 searches/day. Errors arrive in Google's standard envelope
// ({"error":{"code","message","errors":[{"reason"}]}}); a 403 with reason
// quotaExceeded means the daily quota is spent.
const (
	defaultYouTubeURL = "https://www.googleapis.com"
	maxYouTubeCount   = 50 // maxResults ceiling per the spec
)

// youtube is a Searcher over /youtube/v3/search. Each hit is a video, channel,
// or playlist: URL is the watch/channel/playlist link, Site is the channel
// title, Content is the description, and Published is the publish timestamp.
// There is no relevance score in the response, so Score is left unset.
type youtube struct {
	svc     *ytapi.Service
	timeout time.Duration
}

func newYouTube(cfg config.SearchEngine) (*youtube, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("search youtube: api_key is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultYouTubeURL
	}
	// The key is applied by the transport google-api-go-client builds itself.
	// That is also why there is no option.WithHTTPClient here: it "takes
	// precedent over all other supplied options", so pairing it with
	// WithAPIKey silently drops the key and every call comes back 403. The
	// per-engine timeout is enforced on the context instead (see Search).
	opts := []option.ClientOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithEndpoint(base + "/"),
	}
	svc, err := ytapi.NewService(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("search youtube: build service: %w", err)
	}
	return &youtube{svc: svc, timeout: cfg.TimeoutD()}, nil
}

// Search runs one YouTube search (videos only).
func (y *youtube) Search(ctx context.Context, req Request) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, y.timeout)
	defer cancel()
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, fmt.Errorf("search youtube: query is required")
	}
	count := req.Count
	if count <= 0 {
		count = 10
	}
	if count > maxYouTubeCount {
		count = maxYouTubeCount
	}

	call := y.svc.Search.List([]string{"snippet"}).
		Type("video").
		Order("relevance").
		MaxResults(int64(count)).
		Q(query)
	if after, before, ok := youtubeTimeRange(req.TimeRange); ok {
		call = call.PublishedAfter(after)
		if before != "" {
			call = call.PublishedBefore(before)
		}
	}

	resp, err := call.Context(ctx).Do()
	if err != nil {
		if ge, ok := err.(*googleapi.Error); ok {
			return nil, fmt.Errorf("search youtube: %s", youtubeError(ge))
		}
		return nil, fmt.Errorf("search youtube: %w", err)
	}

	out := &Result{Query: query, Results: make([]WebResult, 0, len(resp.Items))}
	for _, it := range resp.Items {
		if it == nil || it.Id == nil || it.Snippet == nil {
			continue
		}
		sn := it.Snippet
		// Titles and descriptions arrive HTML-escaped (&#39;, &quot;, &amp;).
		title := html.UnescapeString(sn.Title)
		desc := html.UnescapeString(sn.Description)
		w := WebResult{
			Title:     title,
			URL:       youtubeURL(it.Id.Kind, it.Id.VideoId, it.Id.ChannelId, it.Id.PlaylistId),
			Site:      sn.ChannelTitle,
			Content:   desc,
			Published: sn.PublishedAt,
		}
		snippet := truncate([]byte(desc), 240)
		switch sn.LiveBroadcastContent {
		case "live":
			snippet = "LIVE: " + snippet
		case "upcoming":
			snippet = "upcoming broadcast: " + snippet
		}
		w.Snippet = snippet
		out.Results = append(out.Results, w)
	}
	return out, nil
}

// youtubeError renders Google's error envelope, with a plain-language hint for
// the common quota-exhaustion case. Never includes the API key.
func youtubeError(e *googleapi.Error) string {
	reason := ""
	if len(e.Errors) > 0 {
		reason = e.Errors[0].Reason
	}
	msg := fmt.Sprintf("status %d: %s", e.Code, e.Message)
	if reason != "" {
		msg += " (reason " + reason + ")"
	}
	if e.Code == http.StatusForbidden && strings.Contains(reason, "quota") {
		msg += " — the daily YouTube Data API quota is spent (search.list costs 100 units/call against the default 10,000/day); it resets at midnight Pacific"
	}
	return msg
}

// youtubeURL maps a search result's id onto its canonical watch/channel/
// playlist URL.
func youtubeURL(kind, videoID, channelID, playlistID string) string {
	switch kind {
	case "youtube#video":
		return "https://www.youtube.com/watch?v=" + videoID
	case "youtube#channel":
		return "https://www.youtube.com/channel/" + channelID
	case "youtube#playlist":
		return "https://www.youtube.com/playlist?list=" + playlistID
	}
	return ""
}

// youtubeTimeRange maps the shared TimeRange enum (and the
// YYYY-MM-DD..YYYY-MM-DD range form) onto publishedAfter/publishedBefore RFC
// 3339 timestamps.
func youtubeTimeRange(tr string) (after, before string, ok bool) {
	if tr == "" {
		return "", "", false
	}
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	switch tr {
	case "OneDay":
		return rfc(time.Now().Add(-24 * time.Hour)), "", true
	case "OneWeek":
		return rfc(time.Now().Add(-7 * 24 * time.Hour)), "", true
	case "OneMonth":
		return rfc(time.Now().Add(-30 * 24 * time.Hour)), "", true
	case "OneYear":
		return rfc(time.Now().Add(-365 * 24 * time.Hour)), "", true
	}
	parts := strings.SplitN(tr, "..", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	f, err1 := time.ParseInLocation("2006-01-02", parts[0], time.UTC)
	t, err2 := time.ParseInLocation("2006-01-02", parts[1], time.UTC)
	if err1 != nil || err2 != nil {
		return "", "", false
	}
	return rfc(f), rfc(t.Add(24 * time.Hour)), true // before is inclusive midnight + 1 day
}
