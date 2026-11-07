// Package noquery stops Bubble Tea from querying the terminal when the binary
// starts.
//
// Bubble Tea v1's package init calls lipgloss.HasDarkBackground(), which puts
// the terminal in no-echo mode, writes an OSC 11 background-color query and a
// cursor-position query, and reads stdin for the replies for up to five
// seconds. That ran for every agentflow command, including version and
// validate: a terminal that does not answer stalled startup by five seconds
// and swallowed whatever was typed meanwhile, and a late answer arrived as
// stray input.
//
// The dashboard only uses fixed 256-color styles, so the answer is never
// needed. Declaring the background here makes lipgloss skip detection, and
// Bubble Tea's init then returns without touching the terminal.
//
// Ordering is what makes this work. Go initializes, among the packages whose
// dependencies are ready, the one with the lowest import path first. This
// package depends on lipgloss but not on Bubble Tea, and
// "agentflow/internal/tui/noquery" sorts before
// "github.com/charmbracelet/bubbletea", so its init always runs first. Keep it
// free of a Bubble Tea import, and keep its import path below
// "github.com/charmbracelet/bubbletea". TestNoTerminalQueryAtStartup in
// internal/tui fails if either rule breaks.
package noquery

import "github.com/charmbracelet/lipgloss"

func init() {
	lipgloss.SetHasDarkBackground(true)
}
