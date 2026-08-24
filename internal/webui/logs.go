package webui

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

// LogRing is a fixed-depth ring of formatted log lines with fan-out
// subscriptions, backing the console's live log tail (SSE).
type LogRing struct {
	mu    sync.Mutex
	depth int
	lines []string
	subs  map[chan string]struct{}
}

func NewLogRing(depth int) *LogRing {
	if depth <= 0 {
		depth = 500
	}
	return &LogRing{depth: depth, subs: map[chan string]struct{}{}}
}

// Push appends a line and broadcasts it to subscribers. Slow subscribers drop
// lines rather than blocking the logging path.
func (r *LogRing) Push(line string) {
	r.mu.Lock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.depth {
		r.lines = r.lines[len(r.lines)-r.depth:]
	}
	for ch := range r.subs {
		select {
		case ch <- line:
		default:
		}
	}
	r.mu.Unlock()
}

// Recent returns a copy of the buffered lines, oldest first.
func (r *LogRing) Recent() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

// Subscribe registers a buffered channel for new lines; the returned func
// unregisters it.
func (r *LogRing) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 64)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.subs, ch)
		close(ch)
		r.mu.Unlock()
	}
}

// TeeHandler is an slog.Handler that forwards to a downstream handler and
// mirrors a formatted one-line copy into the ring.
type TeeHandler struct {
	downstream slog.Handler
	ring       *LogRing
}

func NewTeeHandler(downstream slog.Handler, ring *LogRing) *TeeHandler {
	return &TeeHandler{downstream: downstream, ring: ring}
}

func (h *TeeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.downstream.Enabled(ctx, level)
}

func (h *TeeHandler) Handle(ctx context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Time.Format("15:04:05"))
	sb.WriteString(" " + logLevelTag(r.Level) + " " + r.Message)
	r.Attrs(func(a slog.Attr) bool {
		sb.WriteString(fmt.Sprintf(" %s=%v", a.Key, a.Value))
		return true
	})
	// SSE data: lines must not carry raw newlines.
	line := strings.Map(func(rn rune) rune {
		if rn == '\n' || rn == '\r' {
			return ' '
		}
		return rn
	}, sb.String())
	h.ring.Push(line)
	return h.downstream.Handle(ctx, r)
}

func (h *TeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TeeHandler{downstream: h.downstream.WithAttrs(attrs), ring: h.ring}
}

func (h *TeeHandler) WithGroup(name string) slog.Handler {
	return &TeeHandler{downstream: h.downstream.WithGroup(name), ring: h.ring}
}

func logLevelTag(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERR"
	case l >= slog.LevelWarn:
		return "WRN"
	case l >= slog.LevelInfo:
		return "INF"
	default:
		return "DBG"
	}
}

// handleLogs streams the log tail as SSE: first the ring's recent lines, then
// live lines until the client disconnects.
func (u *UI) handleLogs(w http.ResponseWriter, r *http.Request) {
	if u.deps.Logs == nil {
		writeErr(w, http.StatusServiceUnavailable, "log tail not enabled")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	for _, line := range u.deps.Logs.Recent() {
		fmt.Fprintf(w, "data: %s\n\n", line)
	}
	fl.Flush()

	ch, unsub := u.deps.Logs.Subscribe()
	defer unsub()
	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
			fl.Flush()
		}
	}
}
