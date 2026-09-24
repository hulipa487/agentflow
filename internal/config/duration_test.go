package config

import (
	"strings"
	"testing"
	"time"
)

// TestParseEvery: the every: duration forms and their errors.
func TestParseEvery(t *testing.T) {
	good := map[string]time.Duration{
		"30s":   30 * time.Second,
		"15m":   15 * time.Minute,
		"6h":    6 * time.Hour,
		"1d":    24 * time.Hour,
		"2d12h": 60 * time.Hour,
		"1.5h":  90 * time.Minute,
		"90s":   90 * time.Second,
		" 5m ":  5 * time.Minute,
		"250ms": 250 * time.Millisecond,
		"1m30s": 90 * time.Second,
		"1d30m": 24*time.Hour + 30*time.Minute,
	}
	for in, want := range good {
		got, err := ParseEvery(in)
		if err != nil {
			t.Errorf("ParseEvery(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseEvery(%q) = %v; want %v", in, got, want)
		}
	}
	for _, in := range []string{"", "5", "5x", "abc", "-5m", "0s", "1d5", "5 m", "1h30", "5us", "1w"} {
		if got, err := ParseEvery(in); err == nil {
			t.Errorf("ParseEvery(%q) must fail, got %v", in, got)
		}
	}
}

// TestParseRetention: a store's retention takes the same units as an every:
// interval, plus the "forever" sentinel. It was parsed with time.ParseDuration
// and the error was discarded, and Go has no day unit — so the shipped
// `retention: "30d"` became zero, which reads as "never expires". The field
// looked configured and did nothing at all.
func TestParseRetention(t *testing.T) {
	good := map[string]time.Duration{
		"30d":      30 * 24 * time.Hour,
		"1d":       24 * time.Hour,
		"12h":      12 * time.Hour,
		" 30d ":    30 * 24 * time.Hour,
		"forever":  0,
		"FOREVER":  0,
		"  forever": 0,
	}
	for in, want := range good {
		got, err := ParseRetention(in)
		if err != nil {
			t.Errorf("ParseRetention(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseRetention(%q) = %v; want %v", in, got, want)
		}
	}
	// "never" is not a synonym, and a typo must not read as no-expiry.
	for _, in := range []string{"", "30days", "never", "0d", "-1d"} {
		if got, err := ParseRetention(in); err == nil {
			t.Errorf("ParseRetention(%q) must fail, got %v", in, got)
		}
	}
}

// TestValidateMemoryRetention: a retention that does not parse used to be
// silently zero, which reads as "never expires" — the field looked configured
// and did nothing. A typo is now a boot error naming the field, and the walk
// covers the built-in preset too, since that is where the two shipped spellings
// live.
func TestValidateMemoryRetention(t *testing.T) {
	base := func(ret string) *Config {
		return &Config{
			Agents: map[string]Agent{
				"bot": {Loop: "plugin:per_chat", Memory: MemoryAgentConfig{Profile: "conversational"}},
			},
			Memory: Memory{Backends: map[string]Backend{"db": {Provider: "sqlite"}}},
			Profiles: Profiles{Memory: map[string]MemoryProfile{
				"p": {Stores: map[string]Store{"s": {Backend: "db", Table: "t", Retention: ret}}},
			}},
		}
	}
	for _, ok := range []string{"", "30d", "forever", "12h", "1d12h"} {
		if err := validate("cfg.yaml", base(ok)); err != nil {
			t.Errorf("retention %q should validate: %v", ok, err)
		}
	}
	for _, bad := range []string{"30days", "1w", "never", "0d", "-1d"} {
		err := validate("cfg.yaml", base(bad))
		if err == nil || !strings.Contains(err.Error(), "invalid retention") {
			t.Errorf("retention %q must fail validation, got %v", bad, err)
		}
	}
}

// TestDefaultMemoryProfileRetentionParses: the built-in profile ships
// `retention: "30d"` and `"forever"`, so these two spellings must keep working
// — they are the ones a deployment inherits without writing anything.
func TestDefaultMemoryProfileRetentionParses(t *testing.T) {
	for name, s := range DefaultMemoryProfile().Stores {
		if s.Retention == "" {
			continue
		}
		if _, err := ParseRetention(s.Retention); err != nil {
			t.Errorf("built-in store %q retention %q does not parse: %v", name, s.Retention, err)
		}
	}
}
