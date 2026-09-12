package llog

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"dev": LevelDev, "DEV": LevelDev,
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo, "": slog.LevelInfo,
		"warn": slog.LevelWarn, "warning": slog.LevelWarn,
		"error": slog.LevelError, "err": slog.LevelError,
	} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("trace"); err == nil {
		t.Error("unknown level should error")
	}
}

func TestNameAndShortName(t *testing.T) {
	cases := []struct {
		l     slog.Level
		name  string
		short string
	}{
		{LevelDev, "DEV", "DEV"},
		{LevelDev - 4, "DEV", "DEV"},        // anything at/below -8 is DEV
		{slog.LevelDebug, "DEBUG", "DBG"},
		{slog.LevelDebug + 2, "DEBUG", "DBG"}, // between debug and info
		{slog.LevelInfo, "INFO", "INF"},
		{slog.LevelWarn, "WARN", "WRN"},
		{slog.LevelError, "ERROR", "ERR"},
		{slog.LevelError + 4, "ERROR", "ERR"},
	}
	for _, c := range cases {
		if got := Name(c.l); got != c.name {
			t.Errorf("Name(%d) = %q, want %q", c.l, got, c.name)
		}
		if got := ShortName(c.l); got != c.short {
			t.Errorf("ShortName(%d) = %q, want %q", c.l, got, c.short)
		}
	}
}

// The filter is additive: a configured level prints itself and everything
// above, nothing below.
func TestAdditiveFiltering(t *testing.T) {
	emit := func(t *testing.T, level slog.Level) string {
		t.Helper()
		var buf bytes.Buffer
		log := slog.New(NewTextHandler(&buf, level))
		Dev(log, "dev line")
		log.Debug("debug line")
		log.Info("info line")
		log.Warn("warn line")
		log.Error("error line")
		return buf.String()
	}

	tests := []struct {
		level    slog.Level
		present  []string
		absent   []string
	}{
		{LevelDev, []string{"dev line", "debug line", "info line", "warn line", "error line"}, nil},
		{slog.LevelDebug, []string{"debug line", "info line", "warn line", "error line"}, []string{"dev line"}},
		{slog.LevelInfo, []string{"info line", "warn line", "error line"}, []string{"dev line", "debug line"}},
		{slog.LevelWarn, []string{"warn line", "error line"}, []string{"dev line", "debug line", "info line"}},
		{slog.LevelError, []string{"error line"}, []string{"dev line", "debug line", "info line", "warn line"}},
	}
	for _, tt := range tests {
		out := emit(t, tt.level)
		for _, want := range tt.present {
			if !strings.Contains(out, want) {
				t.Errorf("level %d: missing %q in\n%s", tt.level, want, out)
			}
		}
		for _, unwanted := range tt.absent {
			if strings.Contains(out, unwanted) {
				t.Errorf("level %d: unexpected %q in\n%s", tt.level, unwanted, out)
			}
		}
	}
}

// The text handler renders DEV as "DEV", not slog's default "DEBUG-4".
func TestDevRendersCleanly(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewTextHandler(&buf, LevelDev))
	Dev(log, "scratch")
	out := buf.String()
	if !strings.Contains(out, "level=DEV") {
		t.Errorf("DEV should render by name: %q", out)
	}
	if strings.Contains(out, "DEBUG-4") {
		t.Errorf("raw level arithmetic leaked: %q", out)
	}
}
