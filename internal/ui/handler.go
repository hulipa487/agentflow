package ui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// TeeHandler is an slog.Handler that forwards every record to a downstream
// handler (e.g. a file or null sink) and mirrors a formatted copy into the TUI
// log pane. When the TUI is enabled it replaces the stderr text handler so log
// lines appear inside the dashboard instead of corrupting the screen.
type TeeHandler struct {
	downstream slog.Handler
	push       func(string)
}

// NewTeeHandler wraps downstream and mirrors formatted lines via push.
func NewTeeHandler(downstream slog.Handler, push func(string)) *TeeHandler {
	return &TeeHandler{downstream: downstream, push: push}
}

func (h *TeeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.downstream.Enabled(ctx, level)
}

func (h *TeeHandler) Handle(ctx context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Time.Format("15:04:05"))
	sb.WriteString(" " + levelTag(r.Level) + " " + r.Message)
	r.Attrs(func(a slog.Attr) bool {
		sb.WriteString(fmt.Sprintf(" %s=%v", a.Key, a.Value))
		return true
	})
	h.push(sb.String())
	return h.downstream.Handle(ctx, r)
}

func (h *TeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TeeHandler{downstream: h.downstream.WithAttrs(attrs), push: h.push}
}

func (h *TeeHandler) WithGroup(name string) slog.Handler {
	return &TeeHandler{downstream: h.downstream.WithGroup(name), push: h.push}
}

func levelTag(l slog.Level) string {
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
