package browser

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"agentflow/internal/config"
)

// livePause paces the subtests. The Workers Free plan allows one Quick Action
// every 10 seconds, so firing them back to back makes the second one 429
// reliably — a failure that would look like a driver bug rather than a quota.
// A Paid account can set AF_BROWSER_PAUSE=0.
const livePause = 11 * time.Second

// TestLiveBrowserRun verifies the request shapes against a real account. It is
// opt-in because the wire format is otherwise pinned only by this package's own
// httptest fakes checked against the published documentation — which is not the
// same as confirmation.
//
//	AF_BROWSER_ACCOUNT_ID=... AF_BROWSER_API_TOKEN=... \
//	  go test ./internal/drivers/browser/ -run TestLiveBrowserRun -v -timeout 5m
//
// The token needs the "Browser Rendering - Edit" permission. AF_BROWSER_PAUSE
// overrides the inter-request pause (a Go duration; 0 on a Paid account).
func TestLiveBrowserRun(t *testing.T) {
	account := os.Getenv("AF_BROWSER_ACCOUNT_ID")
	token := os.Getenv("AF_BROWSER_API_TOKEN")
	if account == "" || token == "" {
		t.Skip("set AF_BROWSER_ACCOUNT_ID and AF_BROWSER_API_TOKEN to run against live Browser Run")
	}
	pause := livePause
	if v := os.Getenv("AF_BROWSER_PAUSE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("AF_BROWSER_PAUSE %q is not a duration: %v", v, err)
		}
		pause = d
	}
	c := Build(config.Browser{AccountID: account, APIToken: token}, &config.Resolver{}, nil)
	if c.Empty() {
		t.Fatal("client must build from the environment credentials")
	}
	ctx := context.Background()
	first := true
	between := func() {
		if first {
			first = false
			return
		}
		time.Sleep(pause)
	}

	t.Run("markdown", func(t *testing.T) {
		between()
		res, err := c.Do(ctx, Request{Action: ActionMarkdown, URL: "https://example.com"})
		if err != nil {
			t.Fatalf("markdown: %v", err)
		}
		if !strings.Contains(res.Markdown, "Example Domain") {
			t.Fatalf("markdown = %q", truncate([]byte(res.Markdown), 200))
		}
	})

	t.Run("content", func(t *testing.T) {
		between()
		res, err := c.Do(ctx, Request{Action: ActionContent, URL: "https://example.com"})
		if err != nil {
			t.Fatalf("content: %v", err)
		}
		if !strings.Contains(strings.ToLower(res.Content), "<html") {
			t.Fatalf("content = %q", truncate([]byte(res.Content), 200))
		}
	})

	t.Run("links", func(t *testing.T) {
		between()
		res, err := c.Do(ctx, Request{Action: ActionLinks, URL: "https://example.com"})
		if err != nil {
			t.Fatalf("links: %v", err)
		}
		if len(res.Links) == 0 {
			t.Fatal("example.com carries at least one link")
		}
	})

	t.Run("scrape", func(t *testing.T) {
		between()
		res, err := c.Do(ctx, Request{Action: ActionScrape, URL: "https://example.com", Selectors: []string{"h1"}})
		if err != nil {
			t.Fatalf("scrape: %v", err)
		}
		groups, ok := res.Elements.([]any)
		if !ok || len(groups) == 0 {
			t.Fatalf("scrape returned %#v", res.Elements)
		}
	})

	// accessibility_tree is the one action whose documented path is in doubt:
	// Cloudflare's Quick Actions guide page shows browser-run/accessibilityTree
	// while its API reference and every sibling endpoint show
	// browser-rendering. This subtest is what settles it — a 404 here means
	// pathFor needs the other spelling, and its doc comment says so.
	t.Run("accessibility_tree", func(t *testing.T) {
		between()
		res, err := c.Do(ctx, Request{Action: ActionAccessibilityTree, URL: "https://example.com"})
		if err != nil {
			t.Fatalf("accessibility_tree: %v — a 404 means pathFor needs the other "+
				"spelling; see its doc comment", err)
		}
		if res.Tree == nil {
			t.Fatal("accessibility_tree returned no tree")
		}
	})

	// A bad token must fail with Cloudflare's own code and message rather than
	// a bare status, and must not echo the credential back.
	t.Run("error_envelope", func(t *testing.T) {
		between()
		bad := Build(config.Browser{AccountID: account, APIToken: "definitely-not-a-real-token"}, &config.Resolver{}, nil)
		_, err := bad.Do(ctx, Request{Action: ActionMarkdown, URL: "https://example.com"})
		if err == nil {
			t.Fatal("an invalid token must fail")
		}
		if strings.Contains(err.Error(), "definitely-not-a-real-token") {
			t.Fatalf("the token leaked into the error: %v", err)
		}
		if !strings.Contains(err.Error(), "status ") {
			t.Fatalf("the error must name the HTTP status: %v", err)
		}
	})
}
