package scheduler

import (
	"strings"
	"testing"
	"time"
)

// TestParseCronRejectsWhatItCannotHonour: the parser accepted fields it then
// ignored. The day-of-week bound started at index 3, so "0 9 * * 1" (Mondays)
// was accepted and fired every day; the hour was never validated at all; and
// next() discards the hour entirely for a "*/N" minute, so "*/15 9-17 * * *"
// fired around the clock. A schedule the engine cannot honour is a schedule it
// should refuse, not one it silently rewrites.
func TestParseCronRejectsWhatItCannotHonour(t *testing.T) {
	bad := []struct {
		expr string
		want string
	}{
		{"0 9 * * 1", "day-of-month, month and day-of-week"},
		{"0 9 1 * *", "day-of-month, month and day-of-week"},
		{"0 25 * * *", `must be "*" or 0-23`},
		{"0 abc * * *", `must be "*" or 0-23`},
		{"*/15 9 * * *", "cannot be anchored to an hour"},
		{"*/0 * * * *", "N > 0"},
		{"0,30 * * * *", "must be N or */N"},
		{"0-30 * * * *", "must be N or */N"},
	}
	for _, c := range bad {
		_, err := parseCron(c.expr)
		if err == nil {
			t.Errorf("parseCron(%q) must fail", c.expr)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("parseCron(%q) error %q does not mention %q", c.expr, err, c.want)
		}
	}

	// What the engine can honour still parses.
	for _, ok := range []string{"*/15 * * * *", "0 9 * * *", "30 17 * * *"} {
		if _, err := parseCron(ok); err != nil {
			t.Errorf("parseCron(%q) should parse: %v", ok, err)
		}
	}
}

// TestParseCronFixedHourIsUsed is the behaviour the hour validation protects: a
// fixed hour has to actually pin the hour.
func TestParseCronFixedHourIsUsed(t *testing.T) {
	c, err := parseCron("0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	next := c.next(time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC))
	if next.Hour() != 9 || next.Minute() != 0 || !next.After(time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("next = %v; want 09:00 the same day", next)
	}
	next = c.next(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	if next.Hour() != 9 || next.Day() != 25 {
		t.Fatalf("next = %v; want 09:00 the next day", next)
	}
}
