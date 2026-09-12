package search

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/config"
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
	baseURL string
	key     string
	http    *http.Client
}

func newYouTube(cfg config.SearchEngine) (*youtube, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("search youtube: api_key is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultYouTubeURL
	}
	return &youtube{
		baseURL: strings.TrimSuffix(base, "/"),
		key:     cfg.APIKey,
		http:    &http.Client{Timeout: cfg.TimeoutD()},
	}, nil
}

type youtubeResponse struct {
	Items []struct {
		ID struct {
			Kind       string `json:"kind"` // youtube#video | youtube#channel | youtube#playlist
			VideoID    string `json:"videoId"`
			ChannelID  string `json:"channelId"`
			PlaylistID string `json:"playlistId"`
		} `json:"id"`
		Snippet struct {
			Title                string `json:"title"`
			Description          string `json:"description"`
			ChannelTitle         string `json:"channelTitle"`
			PublishedAt          string `json:"publishedAt"`          // RFC 3339
			LiveBroadcastContent string `json:"liveBroadcastContent"` // none|live|upcoming
		} `json:"snippet"`
	} `json:"items"`
	// Standard Google error envelope (present on failures, non-200 status).
	Err *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
}

// Search runs one YouTube search (videos only).
func (y *youtube) Search(ctx context.Context, req Request) (*Result, error) {
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

	q := url.Values{}
	q.Set("part", "snippet")
	q.Set("type", "video")
	q.Set("order", "relevance")
	q.Set("maxResults", strconv.Itoa(count))
	q.Set("q", query)
	q.Set("key", y.key)
	if after, before, ok := youtubeTimeRange(req.TimeRange); ok {
		q.Set("publishedAfter", after)
		if before != "" {
			q.Set("publishedBefore", before)
		}
	}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, y.baseURL+"/youtube/v3/search?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("search youtube: build request: %w", err)
	}
	hreq.Header.Set("Accept", "application/json")

	resp, err := y.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("search youtube: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("search youtube: read response: %w", err)
	}
	var yr youtubeResponse
	if err := json.Unmarshal(payload, &yr); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("search youtube: status %d: %s", resp.StatusCode, truncate(payload, 512))
		}
		return nil, fmt.Errorf("search youtube: decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search youtube: %s", youtubeError(resp.StatusCode, &yr))
	}

	out := &Result{Query: query, Results: make([]WebResult, 0, len(yr.Items))}
	for _, it := range yr.Items {
		sn := it.Snippet
		// Titles and descriptions arrive HTML-escaped (&#39;, &quot;, &amp;).
		title := html.UnescapeString(sn.Title)
		desc := html.UnescapeString(sn.Description)
		w := WebResult{
			Title:     title,
			URL:       youtubeURL(it.ID.Kind, it.ID.VideoID, it.ID.ChannelID, it.ID.PlaylistID),
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
func youtubeError(status int, yr *youtubeResponse) string {
	if yr.Err == nil {
		return fmt.Sprintf("status %d", status)
	}
	reason := ""
	if len(yr.Err.Errors) > 0 {
		reason = yr.Err.Errors[0].Reason
	}
	msg := fmt.Sprintf("status %d: %s", status, yr.Err.Message)
	if reason != "" {
		msg += " (reason " + reason + ")"
	}
	if status == http.StatusForbidden && strings.Contains(reason, "quota") {
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
