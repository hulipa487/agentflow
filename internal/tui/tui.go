// Package ui renders the operator dashboard: a live Bubbletea TUI showing
// per-agent status (active/idle/spawned), the PM→worker project tree, and a
// scrolling log tail. It is fed by a supervisor Snapshot poller and a tee'd
// slog handler; it owns the terminal only when attached to a TTY.
package tui

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"agentflow/internal/core/supervisor"
)

// snapshotMsg carries one supervisor snapshot into the model.
type snapshotMsg struct {
	rows   []supervisor.SessionStatus
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
// when Metrics is nil the stats panel is hidden (tests and minimal setups).
type Source struct {
	Snapshot func() (rows []supervisor.SessionStatus, active, idle int)
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

	rows   []supervisor.SessionStatus
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
		rows, active, idle := src.Snapshot()
		return snapshotMsg{rows: rows, active: active, idle: idle}
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
			for _, d := range statDefs {
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
		m.rows = msg.rows
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
	borderStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("68"))  // muted accent blue
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75"))                             // accent blue (#58a6ff-ish)
	activeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))                                        // ok green
	idleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))                                       // muted gray
	pmStyle     = lipgloss.NewStyle().Bold(true)
	logStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))                                       // dim gray
)

func statusDot(busy bool) string {
	if busy {
		return activeStyle.Render("● active")
	}
	return idleStyle.Render("○ idle")
}

func (m *Model) View() string {
	m.drainPending()
	var b strings.Builder

	// Header / status panel.
	header := fmt.Sprintf("Agents  %s   %s   Σ spawned: %d",
		activeStyle.Render(fmt.Sprintf("● active: %d", m.active)),
		idleStyle.Render(fmt.Sprintf("○ idle: %d", m.idle)),
		len(m.rows))
	b.WriteString(titleStyle.Render("AgentFlow") + "\n")
	b.WriteString(header + "\n\n")

	// Project tree: PMs top-level, workers nested under their parent PM.
	b.WriteString(titleStyle.Render("Projects") + "\n")
	b.WriteString(m.renderTree())
	b.WriteString("\n")

	// Stats panel (only when a metrics source is wired).
	if m.src.Metrics != nil {
		b.WriteString(titleStyle.Render("Stats") + "\n")
		b.WriteString(m.renderStats())
		b.WriteString("\n")
	}

	// Log tail.
	logHeight := m.logHeight()
	b.WriteString(titleStyle.Render("Logs") + "\n")
	start := 0
	if len(m.logs) > logHeight {
		start = len(m.logs) - logHeight
	}
	for _, line := range m.logs[start:] {
		b.WriteString(logStyle.Render(truncate(line, m.width-2)) + "\n")
	}

	b.WriteString(idleStyle.Render("\n q / esc to quit"))
	return borderStyle.Width(m.width - 2).Render(b.String())
}

// logHeight budgets rows for the log pane after the fixed panels.
func (m *Model) logHeight() int {
	// header(2) + blank + projects title + tree lines + blank + logs title + footer
	used := 2 + 1 + 1 + len(m.rows) + 1 + 1 + 2
	if m.src.Metrics != nil {
		used += 1 + len(statDefs) + 1 // stats title + rows + blank
	}
	h := m.height - used - 2
	if h < 3 {
		h = 3
	}
	if h > 12 {
		h = 12
	}
	return h
}

// renderTree builds the PM→worker tree from the flat snapshot. PMs are
// sessions whose agent is the project-manager role (spawned "project_manager"
// or a "pm"-style agent); workers are sessions whose ParentID points at a PM.
// Anything with no parent and not a PM renders as a top-level root row.
func (m *Model) renderTree() string {
	if len(m.rows) == 0 {
		return idleStyle.Render("  (no live sessions)") + "\n"
	}
	children := map[string][]supervisor.SessionStatus{}
	var roots []supervisor.SessionStatus
	for _, r := range m.rows {
		if r.ParentID != "" {
			children[r.ParentID] = append(children[r.ParentID], r)
		} else {
			roots = append(roots, r)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].SessionID < roots[j].SessionID })
	for _, c := range children {
		sort.Slice(c, func(i, j int) bool { return c[i].SessionID < c[j].SessionID })
	}

	var b strings.Builder
	for _, root := range roots {
		kids := children[root.SessionID]
		label := pmStyle.Render(displayID(root.SessionID))
		if len(kids) > 0 {
			b.WriteString(fmt.Sprintf("▾ %s  %s\n", label, statusDot(root.Busy)))
			for i, k := range kids {
				branch := "├─"
				if i == len(kids)-1 {
					branch = "╰─"
				}
				b.WriteString(fmt.Sprintf("  %s %s  %s\n", branch, displayID(k.SessionID), statusDot(k.Busy)))
			}
		} else {
			b.WriteString(fmt.Sprintf("• %s  %s\n", label, statusDot(root.Busy)))
		}
	}
	return b.String()
}

// displayID shortens a session key (agent|route|...) to its agent + tail for
// readability.
func displayID(sessionID string) string {
	parts := strings.Split(sessionID, "|")
	if len(parts) <= 1 {
		return sessionID
	}
	return parts[0] + "|" + parts[len(parts)-1]
}

func truncate(s string, w int) string {
	if w <= 0 || len(s) <= w {
		return s
	}
	return s[:w-1] + "…"
}
