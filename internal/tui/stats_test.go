package tui

import (
	"strings"
	"testing"

	"agentflow/internal/core/metrics"
)

// Every curated row must name a counter the registry actually has. Inc/Add are
// no-ops for unknown names, so an unregistered counter would render a
// permanent zero — a typo here would be invisible at runtime.
func TestCuratedCountersExist(t *testing.T) {
	counters := metrics.DefaultCounters()
	for _, sec := range statSections {
		for _, d := range sec.Rows {
			if _, ok := counters[d.Name]; !ok {
				t.Errorf("section %q: counter %q is not registered in metrics.DefaultCounters",
					sec.Title, d.Name)
			}
		}
	}
}

func TestSparkline(t *testing.T) {
	// Empty input renders nothing.
	if got := sparkline(nil, 10); got != "" {
		t.Errorf("empty input: %q", got)
	}
	// Monotone ramp: last bar must be the tallest block, first the baseline.
	got := sparkline([]int64{0, 1, 2, 3, 4, 5, 6, 7}, 10)
	if !strings.HasSuffix(got, "█") || !strings.HasPrefix(got, "▁") {
		t.Errorf("ramp shape wrong: %q", got)
	}
	// Width cap keeps only the newest samples.
	got = sparkline([]int64{9, 9, 9, 0, 0}, 2)
	if got != "▁▁" {
		t.Errorf("width cap should keep the newest 2 (zeros): %q", got)
	}
	// All-equal values normalize against max=1 without division blowups.
	if got := sparkline([]int64{0, 0, 0}, 5); got != "▁▁▁" {
		t.Errorf("zero series: %q", got)
	}
}

func TestDeltas(t *testing.T) {
	d := deltas([]int64{10, 15, 15, 30})
	if len(d) != 3 || d[0] != 5 || d[1] != 0 || d[2] != 15 {
		t.Errorf("deltas: %v", d)
	}
	// A counter reset (e.g. daily budget reset) clamps to zero, never negative.
	d = deltas([]int64{100, 5})
	if d[0] != 0 {
		t.Errorf("reset should clamp to 0: %v", d)
	}
	if deltas([]int64{7}) != nil {
		t.Error("single sample yields no deltas")
	}
}

func TestHumanize(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0", 42: "42", 9999: "9999", 12000: "12.0k", 3_400_000: "3.4M",
	} {
		if got := humanize(in); got != want {
			t.Errorf("humanize(%d) = %q, want %q", in, got, want)
		}
	}
}

// The metrics pane is sectioned, and every curated section reaches the screen
// at a normal terminal size — including the subsystems the old flat list
// predated (files GC, identity mints).
func TestMetricsPaneRendersSections(t *testing.T) {
	m := sized(testSource(), 120, 40)
	view := m.View()

	if !strings.Contains(view, "Metrics") {
		t.Fatalf("metrics pane title missing:\n%s", view)
	}
	for _, sec := range statSections {
		if !strings.Contains(view, sec.Title) {
			t.Errorf("section %q missing from the pane:\n%s", sec.Title, view)
		}
	}
	// A gauge and a counter both render, with the counter humanized.
	if !strings.Contains(view, "12.3k") {
		t.Errorf("humanized llm calls missing:\n%s", view)
	}
	// Rows from the newest sections prove the extended curation is wired.
	if !strings.Contains(view, "gc swept") || !strings.Contains(view, "minted") {
		t.Errorf("new-subsystem rows missing:\n%s", view)
	}
}

func TestMetricsPaneHiddenWithoutSource(t *testing.T) {
	m := sized(Source{Snapshot: func() (int, int) { return 0, 0 }}, 80, 40)
	if strings.Contains(m.View(), "Metrics") {
		t.Fatal("metrics pane must be hidden when no metrics source is wired")
	}
}

// A counter that has never been incremented (absent from the snapshot) renders
// as 0 with no sparkline rather than panicking.
func TestMetricsPaneHandlesAbsentCounters(t *testing.T) {
	src := Source{
		Snapshot: func() (int, int) { return 0, 0 },
		Metrics:  func() map[string]int64 { return map[string]int64{} },
	}
	m := sized(src, 120, 40)
	view := m.View()
	if !strings.Contains(view, "gc swept") {
		t.Errorf("absent counter row should still render:\n%s", view)
	}
	if !strings.Contains(view, "0") {
		t.Errorf("absent counter should render as 0:\n%s", view)
	}
}

// A short terminal clips the metrics pane and says so, instead of silently
// dropping rows.
func TestMetricsPaneClipsWithMarker(t *testing.T) {
	m := sized(testSource(), 80, 18)
	view := m.View()
	if !strings.Contains(view, "more rows") {
		t.Errorf("clipped pane must mark the clip:\n%s", view)
	}
	// The clipped block still respects its height budget.
	metricsH, _ := m.layout()
	if got := len(strings.Split(m.renderMetrics(metricsH), "\n")); got > metricsH {
		t.Errorf("clipped metrics pane is %d lines, budget %d", got, metricsH)
	}
}

func TestLayoutCols(t *testing.T) {
	for _, tc := range []struct{ usable, cols, colW int }{
		{78, 2, 38},  // 80-col terminal
		{118, 3, 38}, // 120-col terminal
		{218, 3, 71}, // very wide: capped at 3, longer sparklines
		{36, 1, 36},  // too narrow to split
		{20, 1, 20},  // degenerate
	} {
		cols, colW := layoutCols(tc.usable)
		if cols != tc.cols || colW != tc.colW {
			t.Errorf("layoutCols(%d) = (%d,%d), want (%d,%d)", tc.usable, cols, colW, tc.cols, tc.colW)
		}
	}
}

// Packing keeps sections whole and in reading order, and balances the columns.
func TestPackBlocksKeepsSectionsWhole(t *testing.T) {
	blocks := [][]string{{"a1", "a2"}, {"b1", "b2"}, {"c1", "c2"}}

	packed := packBlocks(blocks, 2)
	if len(packed) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(packed))
	}
	var flat []string
	for _, col := range packed {
		for _, line := range col {
			if line != "" { // drop the inter-section separators
				flat = append(flat, line)
			}
		}
	}
	want := []string{"a1", "a2", "b1", "b2", "c1", "c2"}
	if strings.Join(flat, ",") != strings.Join(want, ",") {
		t.Errorf("packing reordered or split sections: %v", flat)
	}

	// One column: every block present, separated by blanks.
	packed = packBlocks(blocks, 1)
	if len(packed) != 1 || len(packed[0]) != 8 { // 6 lines + 2 separators
		t.Errorf("single-column packing wrong: %v", packed)
	}
}

func TestPollMetricsNilSafe(t *testing.T) {
	// No metrics source: pollMetrics must return nil (tea.Batch ignores it).
	src := Source{Snapshot: func() (int, int) { return 0, 0 }}
	if cmd := pollMetrics(src); cmd != nil {
		t.Fatal("expected nil cmd without a metrics source")
	}
	// With a source: the produced message carries latest + spark windows.
	cmd := pollMetrics(testSource())
	if cmd == nil {
		t.Fatal("expected a poll cmd")
	}
	msg, ok := cmd().(metricsMsg)
	if !ok {
		t.Fatalf("poll returned %T, want metricsMsg", msg)
	}
	if msg.latest["agentflow_sessions_active"] != 3 {
		t.Errorf("latest wrong: %v", msg.latest)
	}
	if len(msg.sparks["agentflow_llm_calls"]) != 4 {
		t.Errorf("spark window missing: %v", msg.sparks)
	}
}
