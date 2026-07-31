package ui

import (
	"strings"
	"testing"

	"agentflow/internal/core/supervisor"
)

func TestRenderTreeNestsWorkersUnderPM(t *testing.T) {
	m := New(Source{Snapshot: func() ([]supervisor.SessionStatus, int, int) {
		return nil, 0, 0
	}})
	m.width = 60
	m.height = 30
	m.rows = []supervisor.SessionStatus{
		{SessionID: "main|webhook:1", Agent: "main", Busy: true},
		{SessionID: "spawn:project_manager|main|x", Agent: "spawn:project_manager", ParentID: "", Busy: true},
		{SessionID: "spawn:worker|pm|a", Agent: "spawn:worker", ParentID: "spawn:project_manager|main|x", Busy: true},
		{SessionID: "spawn:worker|pm|b", Agent: "spawn:worker", ParentID: "spawn:project_manager|main|x", Busy: false},
	}
	m.active = 3
	m.idle = 1

	tree := m.renderTree()
	// The worker rows must appear nested (indented with a branch char) under
	// the PM root, not as siblings.
	if !strings.Contains(tree, "spawn:project_manager|x") {
		t.Errorf("PM root missing from tree:\n%s", tree)
	}
	for _, w := range []string{"spawn:worker|a", "spawn:worker|b"} {
		if !strings.Contains(tree, w) {
			t.Errorf("worker %s missing from tree:\n%s", w, tree)
		}
	}
	if !strings.Contains(tree, "├─") && !strings.Contains(tree, "╰─") {
		t.Errorf("expected a branch connector for nested workers:\n%s", tree)
	}
}

func TestSnapshotCounts(t *testing.T) {
	rows := []supervisor.SessionStatus{
		{SessionID: "a", Busy: true},
		{SessionID: "b", Busy: false},
		{SessionID: "c", Busy: true},
	}
	src := Source{Snapshot: func() ([]supervisor.SessionStatus, int, int) {
		active, idle := 0, 0
		for _, r := range rows {
			if r.Busy {
				active++
			} else {
				idle++
			}
		}
		return rows, active, idle
	}}
	m := New(src)
	_ = m
	msg := poll(src)()
	sm, ok := msg.(snapshotMsg)
	if !ok {
		t.Fatalf("poll returned %T, want snapshotMsg", msg)
	}
	if sm.active != 2 || sm.idle != 1 || len(sm.rows) != 3 {
		t.Errorf("snapshot counts wrong: active=%d idle=%d rows=%d", sm.active, sm.idle, len(sm.rows))
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short string changed: %q", got)
	}
	long := strings.Repeat("x", 50)
	got := truncate(long, 20)
	if len(got) <= 20 { // 20 includes the multi-byte ellipsis; check the cut
		if !strings.HasSuffix(got, "…") {
			t.Errorf("expected ellipsis terminator, got %q", got)
		}
	}
	if !strings.HasPrefix(got, "xxxxxxxx") || !strings.HasSuffix(got, "…") {
		t.Errorf("unexpected truncation: %q", got)
	}
}

// A pushed line must land in m.logs exactly once, whether pushed before the
// program starts (buffered) or after (sent to the update loop). Regression for
// the duplicated-log-lines bug where PushLog did both.
func TestPushLogDeduplicates(t *testing.T) {
	m := New(Source{Snapshot: func() ([]supervisor.SessionStatus, int, int) { return nil, 0, 0 }})

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
