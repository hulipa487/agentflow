// Package ui renders the operator dashboard: a live Bubbletea TUI showing
// runtime metrics — the curated counters, sectioned, with sparklines — plus a
// short log tail. It is fed by a supervisor snapshot poller, the counter
// registry, and a tee'd slog handler; it owns the terminal only when attached
// to a TTY.
//
// It is deliberately a read-only metrics display: no operator actions, no chat
// surface, no identity of its own. Interaction and configuration are the web
// console's job, and a new engine subsystem shows up here as a counter row.
package tui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// snapshotMsg carries one supervisor snapshot's session counts into the model.
type snapshotMsg struct {
	active int
	idle   int
}

// metricsMsg carries the latest counter values plus per-counter sample
// windows for the stats panel.
type metricsMsg struct {
	latest map[string]int64
	sparks map[string][]int64
}

// logMsg carries one formatted slog line into the model.
type logMsg string

// tickMsg drives the periodic snapshot poll.
type tickMsg time.Time

// Source provides the data the TUI renders. Metrics and Spark are optional:
// when Metrics is nil the metrics pane is hidden (tests and minimal setups).
type Source struct {
	// Snapshot reports live session counts (busy vs idle) for the header.
	Snapshot func() (active, idle int)
	// Metrics returns the latest value per counter.
	Metrics func() map[string]int64
	// Spark returns recent samples for one counter, oldest first.
	Spark func(name string) []int64
}

// Model is the Bubbletea model for the dashboard.
type Model struct {
	src     Source
	mu      sync.Mutex
	pending []string // log lines buffered before/without a running program

	active int
	idle   int
	latest map[string]int64
	sparks map[string][]int64
	logs   []string
	width  int
	height int
	prog   *tea.Program
}

const maxLogs = 200

// Default terminal geometry used before the first WindowSizeMsg arrives.
const (
	defaultWidth  = 80
	defaultHeight = 24
)

// New builds the model. prog is set later via SetProgram once the program
// starts (the log tee needs it to push lines).
func New(src Source) *Model {
	return &Model{src: src}
}

// SetProgram lets the log tee push lines into the running program.
func (m *Model) SetProgram(p *tea.Program) {
	m.mu.Lock()
	m.prog = p
	m.mu.Unlock()
}

// PushLog is called by the slog tee for every record. Before the program
// starts (prog == nil) lines buffer in pending and flush on the first frame;
// once running they go straight to the update loop. It must never do both for
// one line or every record renders twice.
func (m *Model) PushLog(line string) {
	m.mu.Lock()
	prog := m.prog
	if prog == nil {
		m.pending = append(m.pending, line)
	}
	m.mu.Unlock()
	if prog != nil {
		prog.Send(logMsg(line))
	}
}

func (m *Model) drainPending() {
	m.mu.Lock()
	if len(m.pending) > 0 {
		m.logs = append(m.logs, m.pending...)
		m.pending = m.pending[:0]
		if len(m.logs) > maxLogs {
			m.logs = m.logs[len(m.logs)-maxLogs:]
		}
	}
	m.mu.Unlock()
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(poll(m.src), pollMetrics(m.src), tick())
}

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func poll(src Source) tea.Cmd {
	return func() tea.Msg {
		active, idle := src.Snapshot()
		return snapshotMsg{active: active, idle: idle}
	}
}

// pollMetrics samples the counters and sparkline windows. It returns nil when
// the source has no metrics, which tea.Batch safely ignores.
func pollMetrics(src Source) tea.Cmd {
	if src.Metrics == nil {
		return nil
	}
	return func() tea.Msg {
		msg := metricsMsg{latest: src.Metrics()}
		if src.Spark != nil {
			msg.sparks = map[string][]int64{}
			for _, d := range statDefs() {
				msg.sparks[d.Name] = src.Spark(d.Name)
			}
		}
		return msg
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		return m, tea.Batch(poll(m.src), pollMetrics(m.src), tick())
	case snapshotMsg:
		m.active = msg.active
		m.idle = msg.idle
	case metricsMsg:
		m.latest = msg.latest
		m.sparks = msg.sparks
	case logMsg:
		m.logs = append(m.logs, string(msg))
		if len(m.logs) > maxLogs {
			m.logs = m.logs[len(m.logs)-maxLogs:]
		}
	}
	return m, nil
}

// Palette mirrors the web UIs' shared tokens (internal/webui/static/shared/
// tokens.css): accent blue for titles/borders, green for active/ok, grays for
// muted/idle/log — so the terminal reads as the same product as the console.
var (
	borderStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("68")) // muted accent blue
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75"))                             // accent blue (#58a6ff-ish)
	activeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))                                        // ok green
	idleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))                                       // muted gray
	logStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))                                       // dim gray
)

func (m *Model) View() string {
	m.drainPending()
	// The first frame can render before the terminal size is known.
	if m.width <= 0 {
		m.width = defaultWidth
	}
	if m.height <= 0 {
		m.height = defaultHeight
	}

	var b strings.Builder

	// Header: the live session counts, the one signal the log tail cannot give.
	b.WriteString(titleStyle.Render("AgentFlow") + "\n")
	b.WriteString(fmt.Sprintf("%s   %s\n\n",
		activeStyle.Render(fmt.Sprintf("● active: %d", m.active)),
		idleStyle.Render(fmt.Sprintf("○ idle: %d", m.idle))))

	metricsH, logH := m.layout()
	if m.src.Metrics != nil && metricsH > 0 {
		b.WriteString(titleStyle.Render("Metrics") + "\n")
		b.WriteString(m.renderMetrics(metricsH) + "\n\n")
	}

	// Log footer, sized to the terminal (see layout).
	b.WriteString(titleStyle.Render("Logs") + "\n")
	start := 0
	if len(m.logs) > logH {
		start = len(m.logs) - logH
	}
	for _, line := range m.logs[start:] {
		b.WriteString(logStyle.Render(truncate(line, m.width-2)) + "\n")
	}

	b.WriteString(idleStyle.Render("\n q / esc to quit"))
	return borderStyle.Width(m.width - 2).Render(b.String())
}

// Frame chrome in rendered lines, excluding pane content: title, header,
// blank, pane titles, the blank after the metrics block, the footer line and
// the two border rows.
const (
	chromeWithMetrics = 10
	chromeLogsOnly    = 8
)

// layout budgets the two panes' content lines for the current terminal size.
// The log footer is sized first (a quarter of the terminal, 3–8 lines) and the
// metrics pane takes the remainder. On a terminal too short for both the
// footer shrinks, then the metrics pane is dropped to zero and View skips it —
// so the frame always fits the terminal it was given.
func (m *Model) layout() (metricsH, logH int) {
	h := m.height
	if h <= 0 {
		h = defaultHeight
	}

	if m.src.Metrics == nil {
		logH = h / 4
		if logH < 3 {
			logH = 3
		}
		if logH > 8 {
			logH = 8
		}
		if max := h - chromeLogsOnly; logH > max {
			logH = max
		}
		if logH < 0 {
			logH = 0
		}
		return 0, logH
	}

	// Room for the chrome, a three-line footer and at least one metric row.
	if h >= chromeWithMetrics+4 {
		logH = h / 4
		if logH < 3 {
			logH = 3
		}
		if logH > 8 {
			logH = 8
		}
		if metricsH = h - chromeWithMetrics - logH; metricsH < 1 {
			metricsH = 1
			logH = h - chromeWithMetrics - metricsH
		}
		return metricsH, logH
	}

	// No room for metrics: header and log footer only.
	logH = h - chromeLogsOnly
	if logH < 0 {
		logH = 0
	}
	return 0, logH
}
