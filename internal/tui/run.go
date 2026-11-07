package tui

import (
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
)

// How long Stop waits for Bubble Tea to hand the terminal back. A clean quit
// takes milliseconds; the bounds only matter if the program is wedged.
const (
	quitGrace = 2 * time.Second
	killGrace = time.Second
)

// tailOnExit is how many of the last log lines Stop reprints on the restored
// terminal, so the operator can see how the engine ended (a boot error logged
// while the dashboard owned the screen would otherwise vanish with it).
const tailOnExit = 12

// Dashboard owns the running TUI program. Start launches it; Stop quits it
// and does not return until the terminal is back in the state it was in.
type Dashboard struct {
	model *Model
	prog  *tea.Program
	done  chan struct{} // closed once Run has returned (terminal restored)

	// saved is the terminal mode from before Bubble Tea switched it to raw:
	// the fallback restore if the program cannot shut down by itself.
	saved   *term.State
	savedFd uintptr
	out     io.Writer

	stopOnce sync.Once
}

// Start launches the dashboard in the background. It takes over the terminal
// (raw mode, alternate screen, hidden cursor); Stop gives it back.
func Start(src Source) *Dashboard {
	return start(src, os.Stderr, tea.WithAltScreen())
}

// start is Start with the restored-terminal writer and program options
// injectable, so tests can run a program without a terminal.
func start(src Source, out io.Writer, opts ...tea.ProgramOption) *Dashboard {
	m := New(src)
	p := tea.NewProgram(m, opts...)
	m.SetProgram(p)
	d := &Dashboard{model: m, prog: p, done: make(chan struct{}), out: out}
	if fd := os.Stdin.Fd(); term.IsTerminal(fd) {
		if st, err := term.GetState(fd); err == nil {
			d.saved, d.savedFd = st, fd
		}
	}
	go func() {
		defer close(d.done)
		_, _ = p.Run()
		// Run returns only after Bubble Tea has restored the terminal. Show
		// how things ended; from here on log lines go straight to stderr.
		m.finish(d.out, tailOnExit)
		// The UI and the engine live and die together: a clean quit, a
		// terminal error or a panic all take the engine down instead of
		// leaving a headless process with its logs discarded.
		m.requestQuit()
	}()
	return d
}

// LogHandler returns an slog handler that mirrors every record into the log
// pane while forwarding the original to downstream (use a discard handler when
// the TUI owns the screen, or a file handler to also keep a log file).
func (d *Dashboard) LogHandler(downstream slog.Handler) *TeeHandler {
	return NewTeeHandler(downstream, d.model.PushLog)
}

// Stop quits the program and blocks until the terminal is restored: cooked
// mode with echo, the main screen, a visible cursor. It must run before the
// process exits, including on os.Exit paths, which skip deferred calls.
//
// Returning before Bubble Tea finished its shutdown was the bug this guards
// against: main returned, the process exited mid-shutdown, and the shell was
// left in raw mode on the alternate screen, with typed text invisible and
// output drawn without carriage returns.
//
// Stop is safe to call more than once and from any goroutine.
func (d *Dashboard) Stop() {
	if d == nil || d.prog == nil {
		return
	}
	d.stopOnce.Do(d.stop)
}

func (d *Dashboard) stop() {
	d.prog.Quit()
	if !waitClosed(d.done, quitGrace) {
		// A wedged update loop never processes the quit message. Kill
		// cancels the program, and Bubble Tea still restores the terminal on
		// that path.
		d.prog.Kill()
		if !waitClosed(d.done, killGrace) {
			d.forceRestore()
			d.model.finish(d.out, tailOnExit)
		}
	}
}

func waitClosed(ch <-chan struct{}, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}

// forceRestore puts the terminal back without Bubble Tea's help: the saved
// termios (or console mode), then the escape sequences that leave the
// alternate screen and undo the modes Bubble Tea enabled.
func (d *Dashboard) forceRestore() {
	if d.saved != nil {
		_ = term.Restore(d.savedFd, d.saved)
	}
	_, _ = io.WriteString(d.out, restoreSequence)
}

// restoreSequence undoes what the dashboard turns on: bracketed paste, the
// hidden cursor and the alternate screen, then resets attributes.
const restoreSequence = "\x1b[?2004l" + // bracketed paste off
	"\x1b[?25h" + // show cursor
	"\x1b[?1049l" + // leave the alternate screen
	"\x1b[0m" // reset colors and attributes
