package ui

import (
	"fmt"
	"strconv"
	"strings"
)

// statDef describes one row of the stats panel: which registry counter to
// show, a short label, and whether the value is a gauge (sessions, pending
// requests — sparkline of raw samples) or a monotonic counter (totals —
// sparkline of per-sample deltas, since the raw value only ever climbs).
type statDef struct {
	Name  string
	Label string
	Gauge bool
}

// statDefs is the curated set of operationally relevant counters, in display
// order. The full set stays available via /metrics and the web console.
var statDefs = []statDef{
	{"agentflow_sessions_active", "sessions", true},
	{"agentflow_requests_pending", "pending", true},
	{"agentflow_ingress_total", "ingress", false},
	{"agentflow_egress_total", "egress", false},
	{"agentflow_egress_failed", "egress fail", false},
	{"agentflow_llm_calls", "llm calls", false},
	{"agentflow_llm_tokens", "llm tokens", false},
	{"agentflow_budget_denied", "budget denied", false},
	{"agentflow_safety_drops", "safety drops", false},
	{"agentflow_media_ingested", "media in", false},
}

// sparkBlocks are the 8-level unicode bar chars used for sparklines.
var sparkBlocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// sparkline renders vals (oldest first) as a unicode-bar sparkline capped at
// width chars. Values are normalized against the window max so the shape is
// what carries information; the absolute number is printed beside it.
func sparkline(vals []int64, width int) string {
	if width <= 0 || len(vals) == 0 {
		return ""
	}
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	max := int64(1)
	for _, v := range vals {
		if v > max {
			max = v
		}
	}
	var b strings.Builder
	for _, v := range vals {
		if v < 0 {
			v = 0
		}
		idx := int(v * int64(len(sparkBlocks)-1) / max)
		b.WriteRune(sparkBlocks[idx])
	}
	return b.String()
}

// deltas converts cumulative counter samples to per-sample increments.
// Negative deltas (counter reset — process restart, daily budget reset) clamp
// to zero rather than drawing a misleading dip below the baseline.
func deltas(vals []int64) []int64 {
	if len(vals) < 2 {
		return nil
	}
	out := make([]int64, 0, len(vals)-1)
	for i := 1; i < len(vals); i++ {
		d := vals[i] - vals[i-1]
		if d < 0 {
			d = 0
		}
		out = append(out, d)
	}
	return out
}

// humanize renders a counter value compactly (12k, 3.4M) for the panel.
func humanize(v int64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(v)/1e6)
	case v >= 10_000:
		return fmt.Sprintf("%.1fk", float64(v)/1e3)
	default:
		return strconv.FormatInt(v, 10)
	}
}

// sparkWidth budgets the sparkline column from the terminal width.
func (m *Model) sparkWidth() int {
	w := m.width - 26
	if w < 10 {
		w = 10
	}
	if w > 40 {
		w = 40
	}
	return w
}

// renderStats draws the stats panel: one row per statDef with the latest
// value and a sparkline (deltas for counters, raw for gauges).
func (m *Model) renderStats() string {
	var b strings.Builder
	sw := m.sparkWidth()
	for _, d := range statDefs {
		series := m.sparks[d.Name]
		if !d.Gauge {
			series = deltas(series)
		}
		line := fmt.Sprintf("  %-14s %8s  %s", d.Label, humanize(m.latest[d.Name]), sparkline(series, sw))
		b.WriteString(logStyle.Render(line) + "\n")
	}
	return b.String()
}
