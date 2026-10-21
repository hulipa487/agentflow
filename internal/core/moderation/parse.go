package moderation

import (
	"encoding/json"
	"fmt"
	"strings"

	"agentflow/internal/core/safety"
)

type rawVerdict struct {
	Label  string  `json:"label"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

// parseVerdict extracts the single JSON object a classifier is allowed to
// return. A model that answers with prose, an unknown label, or a score
// outside 0..1 is an error; the filter's on_error policy decides what happens.
func parseVerdict(text string, labels map[string]string) (safety.Verdict, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return safety.Verdict{}, fmt.Errorf("classifier returned no JSON object")
	}
	var raw rawVerdict
	if err := json.Unmarshal([]byte(text[start:end+1]), &raw); err != nil {
		return safety.Verdict{}, fmt.Errorf("classifier returned invalid JSON: %w", err)
	}
	label := strings.ToLower(strings.TrimSpace(raw.Label))
	canonical, ok := labels[label]
	if !ok || canonical == "" {
		return safety.Verdict{}, fmt.Errorf("classifier returned unknown label %q", raw.Label)
	}
	if raw.Score < 0 || raw.Score > 1 {
		return safety.Verdict{}, fmt.Errorf("classifier returned score %v outside 0..1", raw.Score)
	}
	reason := strings.TrimSpace(raw.Reason)
	if len(reason) > 200 {
		reason = reason[:200]
	}
	return safety.Verdict{Label: canonical, Score: raw.Score, Reason: reason}, nil
}
