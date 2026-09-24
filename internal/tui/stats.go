package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// statDef describes one row of the metrics pane: which registry counter to
// show, a short label, and whether the value is a gauge (sessions, pending
// requests — sparkline of raw samples) or a monotonic counter (totals —
// sparkline of per-sample deltas, since the raw value only ever climbs).
type statDef struct {
	Name  string
	Label string
	Gauge bool
}

// statSection groups rows under a title. Sections are the pane's reading
// order; renderMetrics packs them into columns by terminal width, keeping
// each section's rows contiguous so a section is never split across columns.
type statSection struct {
	Title string
	Rows  []statDef
}

// statSections is the curated display set, grouped by subsystem. Every
// registry counter stays available via /metrics and the web console; this is
// the operator's at-a-glance cut. A new engine subsystem earns a row here —
// the TUI is a metrics display, so new surfaces become rows, not panes.
//
// Order is operational priority: what an operator checks first sits first. A
// short terminal clips the tail and marks the clip, so the last sections are
// the ones it is cheapest to lose.
var statSections = []statSection{
	{"Sessions", []statDef{
		{"agentflow_sessions_active", "active", true},
		{"agentflow_children_spawned", "spawned", false},
		{"agentflow_children_died", "died", false},
	}},
	{"LLM", []statDef{
		{"agentflow_llm_calls", "calls", false},
		{"agentflow_llm_tokens", "tokens", false},
		{"agentflow_budget_denied", "budget denied", false},
		{"agentflow_user_quota_denied", "quota denied", false},
	}},
	{"Traffic", []statDef{
		{"agentflow_ingress_total", "ingress", false},
		{"agentflow_ingress_dropped", "dropped", false},
		{"agentflow_egress_total", "egress", false},
		{"agentflow_egress_failed", "egress fail", false},
		{"agentflow_channel_errors", "chan errors", false},
		{"agentflow_trigger_fires", "triggers", false},
	}},
	{"Safety", []statDef{
		{"agentflow_safety_drops", "drops", false},
		{"agentflow_http_private_blocked", "private blk", false},
		{"agentflow_http_insecure_tls", "insecure tls", false},
	}},
	{"Files", []statDef{
		{"agentflow_files_gc_swept", "gc swept", false},
		{"agentflow_files_scratch_swept", "scratch swept", false},
	}},
	{"Media", []statDef{
		{"agentflow_media_ingested", "ingested", false},
		{"agentflow_media_bytes", "bytes", false},
		{"agentflow_media_unsupported", "unsupported", false},
	}},
	{"Credentials", []statDef{
		{"agentflow_credential_gets", "gets", false},
		{"agentflow_credential_gets_denied", "denied", false},
	}},
	{"Identity", []statDef{
		{"agentflow_identity_mints", "minted", false},
		{"agentflow_user_registrations", "registered", false},
		{"agentflow_user_provisions", "provisioned", false},
		{"agentflow_user_links", "linked", false},
	}},
}

// statDefs flattens the curated sections in display order.
func statDefs() []statDef {
	var out []statDef
	for _, s := range statSections {
		out = append(out, s.Rows...)
	}
	return out
}

// Pane geometry: a row is "  " + label + " " + value + "  " + sparkline.
const (
	labelW    = 14
	valueW    = 8
	rowChrome = 2 + labelW + 1 + valueW + 2 // fixed columns before the spark
	minSparkW = 10
	maxSparkW = 40
	gutter    = 2 // blank columns between metric columns
	maxCols   = 3
)

// layoutCols picks the metric column count and width for a content width.
// Another column is added while one can still hold the label, value and a
// legible sparkline; past maxCols extra width becomes longer sparklines.
func layoutCols(usable int) (cols, colW int) {
	cols = usable / (rowChrome + minSparkW)
	if cols < 1 {
		cols = 1
	}
	if cols > maxCols {
		cols = maxCols
	}
	return cols, (usable - gutter*(cols-1)) / cols
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

// truncate clips s to w terminal cells, ellipsizing when it cuts. It measures
// rune widths rather than bytes, so a wide glyph is never split in half.
func truncate(s string, w int) string {
	return runewidth.Truncate(s, w, "…")
}

// row renders one stat row: label, humanized value, and a sparkline sized to
// the column. A counter with no samples yet renders as 0 with an empty
// sparkline.
func (m *Model) row(d statDef, sparkW int) string {
	series := m.sparks[d.Name]
	if !d.Gauge {
		series = deltas(series)
	}
	return fmt.Sprintf("  %-*s %*s  %s",
		labelW, d.Label, valueW, humanize(m.latest[d.Name]), sparkline(series, sparkW))
}

// sectionLines renders a section: its title, then its rows. Rows stay
// contiguous so the column packer never splits a section.
func (m *Model) sectionLines(sec statSection, sparkW, colW int) []string {
	lines := make([]string, 0, len(sec.Rows)+1)
	lines = append(lines, titleStyle.Render(runewidth.Truncate(sec.Title, colW, "…")))
	for _, d := range sec.Rows {
		lines = append(lines, logStyle.Render(runewidth.Truncate(m.row(d, sparkW), colW, "…")))
	}
	return lines
}

// renderMetrics draws the sectioned metrics pane, packed into one to three
// columns and clipped to the height the layout budgeted for it.
func (m *Model) renderMetrics(height int) string {
	cols, colW := layoutCols(m.width - 2)
	sparkW := colW - rowChrome
	if sparkW > maxSparkW {
		sparkW = maxSparkW
	}
	if sparkW < 3 {
		sparkW = 3
	}

	blocks := make([][]string, 0, len(statSections))
	for _, sec := range statSections {
		blocks = append(blocks, m.sectionLines(sec, sparkW, colW))
	}
	lines := joinColumns(packBlocks(blocks, cols), colW)

	// Clip to the available height. The marker states the clip rather than
	// dropping rows silently — a truncated pane that lies is worse than one
	// that admits it.
	if len(lines) > height {
		if height < 1 {
			return ""
		}
		lines = lines[:height]
		lines[height-1] = idleStyle.Render(runewidth.Truncate("… more rows (resize taller)", colW, "…"))
	}
	return strings.Join(lines, "\n")
}

// packBlocks distributes ordered blocks into at most cols columns, keeping
// each block whole and balancing height: a column takes blocks until it
// reaches its share of the total, then the next column starts.
func packBlocks(blocks [][]string, cols int) [][]string {
	if cols < 2 || len(blocks) == 0 {
		var all []string
		for i, b := range blocks {
			if i > 0 {
				all = append(all, "")
			}
			all = append(all, b...)
		}
		return [][]string{all}
	}

	total := 0
	for _, b := range blocks {
		total += len(b) + 1 // + blank separator between sections
	}
	target := (total + cols - 1) / cols

	out := make([][]string, 0, cols)
	var cur []string
	for _, b := range blocks {
		if len(cur) > 0 && len(cur)+len(b) > target && len(out) < cols-1 {
			out = append(out, cur)
			cur = nil
		}
		if len(cur) > 0 {
			cur = append(cur, "")
		}
		cur = append(cur, b...)
	}
	if len(cur) > 0 || len(out) == 0 {
		out = append(out, cur)
	}
	return out
}

// joinColumns lays packed columns side by side, each padded to colW visible
// columns and separated by a gutter.
func joinColumns(cols [][]string, colW int) []string {
	height := 0
	for _, c := range cols {
		if len(c) > height {
			height = len(c)
		}
	}
	out := make([]string, 0, height)
	for i := 0; i < height; i++ {
		var b strings.Builder
		for ci, c := range cols {
			if ci > 0 {
				b.WriteString(strings.Repeat(" ", gutter))
			}
			cell := ""
			if i < len(c) {
				cell = c[i]
			}
			b.WriteString(cell)
			if pad := colW - lipgloss.Width(cell); pad > 0 {
				b.WriteString(strings.Repeat(" ", pad))
			}
		}
		// Trailing padding is cosmetic; it sits after each cell's style reset,
		// so trimming spaces can never cut an escape sequence.
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return out
}
