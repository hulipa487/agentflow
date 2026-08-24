package tui

import (
	"strings"
	"testing"

	"agentflow/internal/core/supervisor"
)

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

func metricsSource() Source {
	return Source{
		Snapshot: func() ([]supervisor.SessionStatus, int, int) { return nil, 0, 0 },
		Metrics: func() map[string]int64 {
			return map[string]int64{
				"agentflow_sessions_active": 3,
				"agentflow_llm_calls":       12345,
			}
		},
		Spark: func(name string) []int64 {
			if name == "agentflow_llm_calls" {
				return []int64{100, 110, 130, 180}
			}
			return []int64{1, 2, 3}
		},
	}
}

func TestStatsPanelRenders(t *testing.T) {
	m := New(metricsSource())
	m.width = 80
	m.height = 40
	m.latest = m.src.Metrics()
	m.sparks = map[string][]int64{}
	for _, d := range statDefs {
		m.sparks[d.Name] = m.src.Spark(d.Name)
	}

	view := m.View()
	if !strings.Contains(view, "Stats") {
		t.Fatalf("stats section missing:\n%s", view)
	}
	// The gauge shows its humanized value; the counter its delta sparkline.
	if !strings.Contains(view, "sessions") || !strings.Contains(view, "3") {
		t.Errorf("sessions row missing:\n%s", view)
	}
	if !strings.Contains(view, "12.3k") {
		t.Errorf("humanized llm calls missing:\n%s", view)
	}
}

func TestStatsPanelHiddenWithoutSource(t *testing.T) {
	m := New(Source{Snapshot: func() ([]supervisor.SessionStatus, int, int) { return nil, 0, 0 }})
	m.width = 80
	m.height = 40
	if strings.Contains(m.View(), "Stats") {
		t.Fatal("stats panel must be hidden when no metrics source is wired")
	}
}

func TestPollMetricsNilSafe(t *testing.T) {
	// No metrics source: pollMetrics must return nil (tea.Batch ignores it).
	src := Source{Snapshot: func() ([]supervisor.SessionStatus, int, int) { return nil, 0, 0 }}
	if cmd := pollMetrics(src); cmd != nil {
		t.Fatal("expected nil cmd without a metrics source")
	}
	// With a source: the produced message carries latest + spark windows.
	cmd := pollMetrics(metricsSource())
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
