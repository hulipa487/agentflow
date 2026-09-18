package fetch

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/netguard"
)

// TestLivePublicFetch checks the guard from the other side. Every deny-list
// test proves something is refused; none of them proves an ordinary public host
// still works, and over-blocking is the failure mode nobody notices until a
// deployment is broken.
//
// It is opt-in because it needs real network egress — and because a CI box with
// no egress would otherwise fail for a reason that has nothing to do with the
// guard.
//
//	AF_FETCH_LIVE=1 go test ./internal/drivers/fetch/ -run TestLivePublicFetch -v
func TestLivePublicFetch(t *testing.T) {
	if os.Getenv("AF_FETCH_LIVE") == "" {
		t.Skip("set AF_FETCH_LIVE=1 to run against the public internet")
	}
	// The strict policy: this is the configuration a deployment actually runs.
	c := New(netguard.Policy{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := c.Do(ctx, Request{URL: "https://example.com", Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("a public host must be reachable under the guard: %v", err)
	}
	if res.Status != 200 {
		t.Fatalf("status = %d", res.Status)
	}
	if !strings.Contains(strings.ToLower(res.Body), "example") {
		t.Fatalf("unexpected body: %q", truncateForTest(res.Body))
	}
	if !res.TLSVerified {
		t.Fatal("a verified https request must report tls_verified")
	}

	// And the guard is still doing its job on the same client.
	if _, err := c.Do(ctx, Request{URL: "http://169.254.169.254/latest/meta-data/"}); err == nil {
		t.Fatal("cloud metadata must be refused")
	} else if !netguard.IsBlocked(err) {
		t.Fatalf("expected an address refusal, got %v", err)
	}
}

func truncateForTest(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
