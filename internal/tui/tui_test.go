package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// testSource builds a Source with live counts and a metrics snapshot that
// includes the newest counters, so the rendered pane is exercised end to end.
func testSource() Source {
	return Source{
		Snapshot: func() (int, int) { return 3, 12 },
		Metrics: func() map[string]int64 {
			return map[string]int64{
				"agentflow_sessions_active":     3,
				"agentflow_llm_calls":           12345,
				"agentflow_files_gc_swept":      7,
				"agentflow_identity_mints":      2,
				"agentflow_files_scratch_swept": 4,
			}
		},
		Spark: func(name string) []int64 { return []int64{1, 2, 3, 4} },
	}
}

// sized builds a model with the panes already populated, as if a poll had run.
func sized(src Source, w, h int) *Model {
	m := New(src)
	m.width = w
	m.height = h
	m.active, m.idle = src.Snapshot()
	if src.Metrics != nil {
		m.latest = src.Metrics()
		m.sparks = map[string][]int64{}
		for _, d := range statDefs() {
			if src.Spark != nil {
				m.sparks[d.Name] = src.Spark(d.Name)
			}
		}
	}
	return m
}

func TestSnapshotCounts(t *testing.T) {
	src := Source{Snapshot: func() (int, int) { return 2, 1 }}
	msg, ok := poll(src)().(snapshotMsg)
	if !ok {
		t.Fatalf("poll returned %T, want snapshotMsg", msg)
	}
	if msg.active != 2 || msg.idle != 1 {
		t.Errorf("snapshot counts wrong: active=%d idle=%d", msg.active, msg.idle)
	}
}

func TestHeaderShowsLiveCounts(t *testing.T) {
	m := sized(testSource(), 80, 30)
	view := m.View()
	if !strings.Contains(view, "active: 3") || !strings.Contains(view, "idle: 12") {
		t.Errorf("header counts missing:\n%s", view)
	}
}

// The Projects tree is gone: the dashboard is a metrics display, and session
// state is carried by the counters and the header, not a nested tree.
func TestViewHasNoSessionTree(t *testing.T) {
	m := sized(testSource(), 100, 40)
	view := m.View()
	if strings.Contains(view, "Projects") {
		t.Errorf("removed Projects pane still rendered:\n%s", view)
	}
	// The header carries the live counts the tree used to show.
	if !strings.Contains(view, "active: 3") || !strings.Contains(view, "idle: 12") {
		t.Errorf("header should carry the live counts:\n%s", view)
	}
}

// Every rendered line must fit the terminal, and the frame must never grow
// taller than the terminal, across a sweep of realistic sizes.
func TestViewFitsTerminal(t *testing.T) {
	sizes := [][2]int{{80, 24}, {120, 30}, {200, 50}, {40, 12}, {100, 18}}
	for _, s := range sizes {
		m := sized(testSource(), s[0], s[1])
		// A log tail long enough to overflow a short pane.
		for i := 0; i < 40; i++ {
			m.logs = append(m.logs, "14:02:11 INFO a fairly long log line that must be truncated")
		}
		view := m.View()
		lines := strings.Split(view, "\n")
		if len(lines) > s[1] {
			t.Errorf("%dx%d: rendered %d lines, taller than the terminal", s[0], s[1], len(lines))
		}
		for i, line := range lines {
			if w := lipgloss.Width(line); w > s[0] {
				t.Errorf("%dx%d: line %d is %d cells wide:\n%s", s[0], s[1], i, w, line)
			}
		}
	}
}

// The log footer scales with the terminal and stays inside 3–8 lines whenever
// the frame can afford it, while the metrics pane takes the remainder. A
// terminal too short for both drops the metrics pane and keeps the footer, so
// the frame still fits.
func TestPaneBudgets(t *testing.T) {
	for _, tc := range []struct{ h, wantLog int }{
		{40, 8}, {30, 7}, {24, 6}, {18, 4},
	} {
		m := sized(testSource(), 100, tc.h)
		metricsH, logH := m.layout()
		if logH != tc.wantLog {
			t.Errorf("height %d: log footer %d lines, want %d", tc.h, logH, tc.wantLog)
		}
		if metricsH < 1 {
			t.Errorf("height %d: metrics pane collapsed to %d rows", tc.h, metricsH)
		}
	}

	m := sized(testSource(), 80, 12)
	metricsH, logH := m.layout()
	if metricsH != 0 {
		t.Errorf("short terminal should drop the metrics pane, got %d rows", metricsH)
	}
	if logH < 1 {
		t.Errorf("short terminal should keep a log footer, got %d lines", logH)
	}
}

// A pushed line must land in m.logs exactly once, whether pushed before the
// program starts (buffered) or after (sent to the update loop). Regression for
// the duplicated-log-lines bug where PushLog did both.
func TestPushLogDeduplicates(t *testing.T) {
	m := New(Source{Snapshot: func() (int, int) { return 0, 0 }})

	// Before the program: buffered, flushed on first View.
	m.PushLog("early line")
	m.drainPending()
	if countOccurrences(m.logs, "early line") != 1 {
		t.Fatalf("early line should appear once, logs=%v", m.logs)
	}

	// After the program: goes through the update loop as a logMsg.
	um, _ := m.Update(logMsg("live line"))
	m = um.(*Model)
	if countOccurrences(m.logs, "live line") != 1 {
		t.Fatalf("live line should appear once, logs=%v", m.logs)
	}
}

func countOccurrences(logs []string, want string) int {
	n := 0
	for _, l := range logs {
		if l == want {
			n++
		}
	}
	return n
}

// Truncation measures terminal cells, not bytes: a wide glyph must not be cut
// in half, and a line of double-width runes must not overflow its budget.
func TestTruncateIsRuneWidthAware(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short string changed: %q", got)
	}
	if got := truncate(strings.Repeat("x", 50), 20); lipgloss.Width(got) != 20 {
		t.Errorf("ascii truncation width = %d, want 20 (%q)", lipgloss.Width(got), got)
	}
	// Wide runes: 20 cells of "世" is 10 glyphs; a byte-slicing truncator would
	// return 20 bytes = 6 glyphs + half a rune.
	wide := strings.Repeat("世", 20)
	got := truncate(wide, 20)
	if w := lipgloss.Width(got); w > 20 {
		t.Errorf("wide truncation overflowed: %d cells (%q)", w, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis terminator: %q", got)
	}
}
