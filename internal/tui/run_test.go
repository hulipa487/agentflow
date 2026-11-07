package tui

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// syncBuffer is a bytes.Buffer safe for the program goroutine and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func headless(t *testing.T, out io.Writer, quit func()) *Dashboard {
	t.Helper()
	return start(Source{Snapshot: func() (int, int) { return 0, 0 }, Quit: quit}, out,
		tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutSignalHandler())
}

// TestStopWaitsForTheProgram is the regression test for the broken-terminal
// bug: Stop used to return as soon as it had queued the quit, so the process
// could exit before Bubble Tea restored the terminal.
func TestStopWaitsForTheProgram(t *testing.T) {
	d := headless(t, io.Discard, nil)
	time.Sleep(50 * time.Millisecond) // let Run start
	d.Stop()
	select {
	case <-d.done:
	default:
		t.Fatal("Stop returned before the program finished (terminal not yet restored)")
	}
	d.Stop() // idempotent
	var nilDash *Dashboard
	nilDash.Stop() // the exit helper calls Stop without a dashboard
}

func TestExitPrintsTailThenStreamsLogs(t *testing.T) {
	var out syncBuffer
	quitCalls := 0
	d := headless(t, &out, func() { quitCalls++ })
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < tailOnExit+3; i++ {
		d.model.PushLog("line " + string(rune('a'+i)))
	}
	d.model.PushLog("ERR httpd server failed to start")
	d.Stop()

	got := out.String()
	if !strings.Contains(got, "ERR httpd server failed to start") {
		t.Fatalf("the last line logged before exit was not reprinted:\n%s", got)
	}
	if n := strings.Count(strings.TrimSpace(got), "\n") + 1; n != tailOnExit {
		t.Fatalf("tail has %d lines, want %d:\n%s", n, tailOnExit, got)
	}
	if quitCalls != 1 {
		t.Fatalf("engine quit hook ran %d times, want 1", quitCalls)
	}

	// After the dashboard is gone, logs go straight to the terminal.
	d.model.PushLog("INF shutting down")
	if !strings.HasSuffix(strings.TrimSpace(out.String()), "INF shutting down") {
		t.Fatalf("post-exit log line not written:\n%s", out.String())
	}
}

func TestForceRestoreWritesResetSequence(t *testing.T) {
	var out syncBuffer
	d := &Dashboard{model: New(Source{}), out: &out}
	d.forceRestore()
	for _, seq := range []string{"\x1b[?1049l", "\x1b[?25h", "\x1b[?2004l"} {
		if !strings.Contains(out.String(), seq) {
			t.Fatalf("force restore is missing %q", seq)
		}
	}
}
