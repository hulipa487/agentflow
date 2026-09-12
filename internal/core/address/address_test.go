package address

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"agent:planner", "agent:planner"},
		{"agent:worker-1:new", "agent:worker-1:new"},
		{"session:abc_123", "session:abc_123"},
	}
	for _, tc := range cases {
		got, err := Parse(tc.raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.raw, err)
		}
		if got.String() != tc.want {
			t.Fatalf("Parse(%q) = %q, want %q", tc.raw, got.String(), tc.want)
		}
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	for _, raw := range []string{
		"", "agent:", "agent:bad/name", "agent:one:two", "session:",
		"user:", "user:bad|char", "user:bad:char", "user:bad.char", "agent:worker:new:extra",
	} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestParseUser(t *testing.T) {
	got, err := Parse("user:u_8f3a9b2c")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Kind != User || got.User != "u_8f3a9b2c" || got.String() != "user:u_8f3a9b2c" {
		t.Fatalf("unexpected parse: %+v", got)
	}
}

func TestParseSessionWithColons(t *testing.T) {
	// Real runtime session IDs contain colons from route keys (e.g.
	// "bot|webhook-0:user:webhook:alice"). The prefix-based parser must
	// accept the full remainder as the session ID.
	got, err := Parse("session:bot|webhook-0:user:webhook:alice")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Kind != Session || got.Session != "bot|webhook-0:user:webhook:alice" {
		t.Fatalf("unexpected parse: %+v", got)
	}
}
