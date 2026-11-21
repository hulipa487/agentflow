package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"agentflow/internal/config"
)

// TestResolveAdminTokenRegional: a regional cell shares one admin credential.
// With no ADMIN_TOKEN it may use named operators, and must not mint a per-boot
// token that no peer knows; with neither it refuses rather than minting.
func TestResolveAdminTokenRegional(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	got, minted, err := resolveAdminToken("pinned", false, true, log)
	if err != nil || got != "pinned" || minted {
		t.Fatalf("pinned regional token: got %q, minted=%v, %v", got, minted, err)
	}
	got, minted, err = resolveAdminToken("", true, true, log)
	if err != nil || got != "" || minted {
		t.Fatalf("regional with operators: got %q, minted=%v, %v; want no token", got, minted, err)
	}
	if _, _, err := resolveAdminToken("", false, true, log); err == nil {
		t.Fatal("regional with no token and no operators must be refused")
	}
}

func TestWarnTopologyConfig(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// A Postgres runtime store with no topology is the historical fleet shape.
	cfg := &config.Config{Runtime: config.Runtime{Persistence: "postgres://u:p@db:5432/agent"}}
	warnTopologyConfig(cfg, log)
	if !strings.Contains(buf.String(), "runtime.topology is unset") {
		t.Fatalf("undeclared fleet not reported:\n%s", buf.String())
	}

	// A declared single instance says nothing.
	buf.Reset()
	cfg.Runtime.Topology = "single"
	warnTopologyConfig(cfg, log)
	if got := buf.String(); got != "" {
		t.Fatalf("declared single warned:\n%s", got)
	}

	// Regional identity on another PostgreSQL server is a warning, not a
	// refusal, and the message names the setting.
	buf.Reset()
	cfg.Runtime.Topology = "regional"
	cfg.Runtime.Identity.Persistence = "postgres://u:p@identity:5432/agent"
	warnTopologyConfig(cfg, log)
	if !strings.Contains(buf.String(), "runtime.identity.persistence") {
		t.Fatalf("different identity server not reported:\n%s", buf.String())
	}
}

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

// TestResolveAdminToken: a configured credential wins and is usable remotely.
// With none, a token is minted, and minted says so: the caller serves the admin
// plane local-only, because a minted token is written to the log. An empty
// token is "no auth" to metrics.AdminServer.auth, which is how the admin plane
// once ended up open under -no-webui, so the single topology always gets one.
func TestResolveAdminToken(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	got, minted, err := resolveAdminToken("pinned", false, false, log)
	if err != nil || got != "pinned" || minted {
		t.Fatalf("pinned token: got %q, minted=%v, %v", got, minted, err)
	}

	got, minted, err = resolveAdminToken("", false, false, log)
	if err != nil || !minted {
		t.Fatalf("no credential: minted=%v, %v; want a minted token", minted, err)
	}
	if len(got) != 32 { // hex of 16 random bytes
		t.Fatalf("generated token = %q (len %d); want 32 hex chars", got, len(got))
	}
	other, _, err := resolveAdminToken("", false, false, log)
	if err != nil || other == got {
		t.Fatalf("two boots must not share a token (%v)", err)
	}

	// Operators are a configured credential: no logged break-glass token is
	// minted beside them, so nothing weaker than them guards a remote client.
	got, minted, err = resolveAdminToken("", true, false, log)
	if err != nil || got != "" || minted {
		t.Fatalf("operators: got %q, minted=%v, %v; want operators only", got, minted, err)
	}
}
