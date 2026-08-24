// Package llog defines AgentFlow's log taxonomy and level plumbing.
//
// Five levels, additive (a configured level prints itself and everything
// above it — slog's native >= comparison):
//
//	DEV    temporary development logs; never committed on purpose
//	DEBUG  every external interaction: LLM API request, MongoDB insert,
//	       pgvector lookup, telegram update received, tool call, ...
//	INFO   lifecycle: runtime launch, channel registered, webhook set
//	WARN   degraded but continuing: upstream 429/502, retry, channel error
//	ERR    the runtime exits or cannot continue
//
// DEV sits at -8, one step below slog.LevelDebug (-4), so the standard
// HandlerOptions.Level knob filters the whole range with no extra code.
package llog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// LevelDev is below slog.LevelDebug for temporary development logging.
const LevelDev = slog.Level(-8)

// ParseLevel parses a level name (case-insensitive): dev, debug, info, warn,
// error. "" defaults to info.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "dev":
		return LevelDev, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want dev|debug|info|warn|error)", s)
}

// Name renders the canonical level tag. slog would print a raw LevelDev as
// "DEBUG-4"; Name gives every level a clean label.
func Name(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN"
	case l >= slog.LevelInfo:
		return "INFO"
	case l >= slog.LevelDebug:
		return "DEBUG"
	default:
		return "DEV"
	}
}

// ShortName renders the compact tag used by the TUI and web console tees.
func ShortName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERR"
	case l >= slog.LevelWarn:
		return "WRN"
	case l >= slog.LevelInfo:
		return "INF"
	case l >= slog.LevelDebug:
		return "DBG"
	default:
		return "DEV"
	}
}

// replaceAttr rewrites the level attribute to the canonical name for text
// output.
func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.LevelKey {
		if l, ok := a.Value.Any().(slog.Level); ok {
			a.Value = slog.StringValue(Name(l))
		}
	}
	return a
}

// NewTextHandler builds the root text handler at the given level.
func NewTextHandler(w io.Writer, level slog.Level) slog.Handler {
	return slog.NewTextHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: replaceAttr})
}

// Dev logs at LevelDev on log. Use it for temporary development instrumentation
// that should never survive a commit review.
func Dev(log *slog.Logger, msg string, args ...any) {
	log.Log(context.Background(), LevelDev, msg, args...)
}
