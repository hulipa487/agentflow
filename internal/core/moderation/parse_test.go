package moderation

import (
	"testing"
	"time"

	"agentflow/internal/core/safety"
)

func safetyVerdict(label string) safety.Verdict { return safety.Verdict{Label: label, Score: 0.9} }

func labels() map[string]string {
	return map[string]string{"safe": "safe", "violence": "violence"}
}

func TestParseVerdictPlainJSON(t *testing.T) {
	v, err := parseVerdict(`{"label":"violence","score":0.91,"reason":"threat"}`, labels())
	if err != nil {
		t.Fatal(err)
	}
	if v.Label != "violence" || v.Score != 0.91 {
		t.Fatalf("got %+v", v)
	}
}

func TestParseVerdictCodeFence(t *testing.T) {
	v, err := parseVerdict("```json\n{\"label\":\"safe\",\"score\":0.99,\"reason\":\"ok\"}\n```", labels())
	if err != nil {
		t.Fatal(err)
	}
	if v.Label != "safe" {
		t.Fatalf("got %+v", v)
	}
}

func TestParseVerdictRejectsUnknownLabel(t *testing.T) {
	if _, err := parseVerdict(`{"label":"nonsense","score":0.9}`, labels()); err == nil {
		t.Fatal("expected an unknown-label error")
	}
}

func TestParseVerdictRejectsBadScore(t *testing.T) {
	if _, err := parseVerdict(`{"label":"safe","score":1.5}`, labels()); err == nil {
		t.Fatal("expected a score-range error")
	}
}

func TestParseVerdictRejectsProse(t *testing.T) {
	if _, err := parseVerdict("the message is fine", labels()); err == nil {
		t.Fatal("expected a missing-JSON error")
	}
}

func TestCacheExpiry(t *testing.T) {
	c := newVerdictCache(2, time.Millisecond)
	c.put("k", safetyVerdict("safe"))
	time.Sleep(2 * time.Millisecond)
	if _, ok := c.get("k"); ok {
		t.Fatal("an expired entry must miss")
	}
}

func TestCacheLRU(t *testing.T) {
	c := newVerdictCache(1, time.Minute)
	c.put("a", safetyVerdict("safe"))
	c.put("b", safetyVerdict("violence"))
	if _, ok := c.get("a"); ok {
		t.Fatal("a should have been evicted")
	}
	if v, ok := c.get("b"); !ok || v.Label != "violence" {
		t.Fatalf("b missing: %+v %v", v, ok)
	}
}
