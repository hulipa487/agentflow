package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"agentflow/internal/config"
)

// TestMemoryFromConfigRetention: the store's retention reaches the backend as a
// write TTL. It was parsed with time.ParseDuration and the error discarded, and
// Go has no day unit — so the shipped `retention: "30d"` silently became zero,
// which reads as "never expires".
func TestMemoryFromConfigRetention(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"12h", 12 * time.Hour},
		{"forever", 0},
		{"", 0}, // unset: no expiry
	}
	for _, c := range cases {
		got := memoryFromConfig(config.Store{Backend: "b", Table: "t", Retention: c.in})
		if got.Retention != c.want {
			t.Errorf("retention %q -> %v; want %v", c.in, got.Retention, c.want)
		}
	}
}

// TestLoopbackListen: a per-boot admin token is only a secret if the port it
// guards cannot be reached from elsewhere, so this answer decides whether a
// non-loopback listen is refused at boot. It reads the address text alone and
// errs toward "reachable" for anything it cannot vouch for.
func TestLoopbackListen(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9090", true},
		{"127.0.0.1:0", true},
		{"localhost:9090", true},
		{"[::1]:9090", true},

		// An empty host binds every interface, so a wildcard is not private.
		{":9090", false},
		{"0.0.0.0:9090", false},
		{"[::]:9090", false},
		{"192.168.1.5:9090", false},

		// A name is never resolved, so it gets no benefit of the doubt.
		{"admin.internal:9090", false},

		// Not host:port at all.
		{"9090", false},
		{"", false},
	}
	for _, c := range cases {
		if got := loopbackListen(c.addr); got != c.want {
			t.Errorf("loopbackListen(%q) = %v; want %v", c.addr, got, c.want)
		}
	}
}

// TestResolveAdminToken: ADMIN_TOKEN wins outright and may be paired with any
// listen address. With no ADMIN_TOKEN a loopback listen gets a generated
// token, and a reachable one is refused rather than served unauthenticated —
// an empty token is "no auth required" as far as metrics.AdminServer.auth is
// concerned, which is how the admin plane used to end up open under -no-webui.
func TestResolveAdminToken(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	got, err := resolveAdminToken("pinned", "0.0.0.0:9090", log)
	if err != nil || got != "pinned" {
		t.Fatalf("pinned token on a public listen: got %q, %v; want %q, nil", got, err, "pinned")
	}

	got, err = resolveAdminToken("", "127.0.0.1:9090", log)
	if err != nil {
		t.Fatalf("loopback listen: unexpected error %v", err)
	}
	if len(got) != 32 { // hex of 16 random bytes
		t.Fatalf("generated token = %q (len %d); want 32 hex chars", got, len(got))
	}

	// Two boots must not share a token.
	other, err := resolveAdminToken("", "127.0.0.1:9090", log)
	if err != nil {
		t.Fatalf("second generation: %v", err)
	}
	if other == got {
		t.Fatal("two boots generated the same token")
	}

	// The pairing this exists to refuse: a token minted per boot is not a
	// secret once the port is reachable from elsewhere.
	for _, addr := range []string{"0.0.0.0:9090", ":9090", "192.168.1.5:9090"} {
		if _, err := resolveAdminToken("", addr, log); err == nil {
			t.Errorf("listen %q with no ADMIN_TOKEN must be refused", addr)
		}
	}
}
