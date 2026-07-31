package ui

import (
	"log/slog"

	tea "github.com/charmbracelet/bubbletea"
)

// Dashboard owns the running TUI program. Start launches it; Stop quits it.
type Dashboard struct {
	model *Model
	prog  *tea.Program
}

// Start launches the dashboard in the background. It takes over the terminal
// (alt-screen); call Stop to restore it on shutdown.
func Start(src Source) *Dashboard {
	m := New(src)
	p := tea.NewProgram(m, tea.WithAltScreen())
	m.SetProgram(p)
	d := &Dashboard{model: m, prog: p}
	go func() { _, _ = p.Run() }()
	return d
}

// LogHandler returns an slog handler that mirrors every record into the log
// pane while forwarding the original to downstream (use a discard handler when
// the TUI owns the screen, or a file handler to also keep a log file).
func (d *Dashboard) LogHandler(downstream slog.Handler) *TeeHandler {
	return NewTeeHandler(downstream, d.model.PushLog)
}

// Stop quits the program and restores the terminal.
func (d *Dashboard) Stop() {
	if d.prog != nil {
		d.prog.Quit()
	}
}
