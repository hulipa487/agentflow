package triggers

import (
	"testing"
	"time"
)

func utc(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

// TestParseCronNextUTC: field matching and the next-occurrence search, in UTC.
func TestParseCronNextUTC(t *testing.T) {
	cases := []struct {
		name string
		expr string
		from time.Time
		want time.Time
	}{
		{"every 5 minutes", "*/5 * * * *", utc(2026, 1, 1, 0, 0), utc(2026, 1, 1, 0, 5)},
		{"mid-minute start", "*/5 * * * *", utc(2026, 1, 1, 0, 0).Add(30 * time.Second), utc(2026, 1, 1, 0, 5)},
		{"fixed hour later today", "0 9 * * *", utc(2026, 3, 5, 8, 0), utc(2026, 3, 5, 9, 0)},
		{"fixed hour already past", "0 9 * * *", utc(2026, 3, 5, 10, 0), utc(2026, 3, 6, 9, 0)},
		{"weekday range skips the weekend", "30 9 * * 1-5", utc(2026, 3, 6, 10, 0), utc(2026, 3, 9, 9, 30)},
		{"monthly on the 1st", "0 0 1 * *", utc(2026, 3, 5, 0, 0), utc(2026, 4, 1, 0, 0)},
		{"comma list", "0,30 * * * *", utc(2026, 3, 5, 7, 5), utc(2026, 3, 5, 7, 30)},
		{"ranged hours with a step", "0 8-10/2 * * *", utc(2026, 3, 5, 7, 0), utc(2026, 3, 5, 8, 0)},
		{"ranged hours with a step, past the last", "0 8-10/2 * * *", utc(2026, 3, 5, 9, 0), utc(2026, 3, 5, 10, 0)},
		{"N/step means N..max", "0/6 * * * *", utc(2026, 3, 5, 7, 1), utc(2026, 3, 5, 7, 6)},
		// Both day fields restricted: cron's OR rule (the 13th, or any Friday).
		{"dom OR dow", "0 0 13 * 5", utc(2026, 3, 5, 0, 0), utc(2026, 3, 6, 0, 0)},
		{"dom OR dow, the dom branch", "0 0 13 * 5", utc(2026, 3, 6, 1, 0), utc(2026, 3, 13, 0, 0)},
		// Only one restricted: that one decides.
		{"dom only", "0 0 13 * *", utc(2026, 3, 14, 0, 0), utc(2026, 4, 13, 0, 0)},
		{"dow only", "0 0 * * 1", utc(2026, 3, 6, 0, 0), utc(2026, 3, 9, 0, 0)},
		{"sunday as 0", "0 12 * * 0", utc(2026, 3, 6, 0, 0), utc(2026, 3, 8, 12, 0)},
		{"sunday as 7", "0 12 * * 7", utc(2026, 3, 6, 0, 0), utc(2026, 3, 8, 12, 0)},
		{"month field", "0 0 1 12 *", utc(2026, 3, 5, 0, 0), utc(2026, 12, 1, 0, 0)},
		{"leap day", "0 0 29 2 *", utc(2026, 3, 5, 0, 0), utc(2028, 2, 29, 0, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := ParseCron(c.expr, time.UTC)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", c.expr, err)
			}
			if got := s.Next(c.from); !got.Equal(c.want) {
				t.Fatalf("Next(%s) = %s; want %s", c.from.Format(time.RFC3339), got.Format(time.RFC3339), c.want.Format(time.RFC3339))
			}
		})
	}
}

// TestParseCronTimezoneOffset: fields are matched in the instance's fixed
// offset, so the same expression lands at a different instant.
func TestParseCronTimezoneOffset(t *testing.T) {
	plus8 := time.FixedZone("UTC+8", 8*3600)
	s, err := ParseCron("0 9 * * *", plus8)
	if err != nil {
		t.Fatal(err)
	}
	// 00:00Z is 08:00 local, so the next 09:00 local is 01:00Z.
	got := s.Next(utc(2026, 3, 5, 0, 0))
	want := utc(2026, 3, 5, 1, 0)
	if !got.Equal(want) {
		t.Fatalf("09:00 UTC+8 = %s; want %s", got.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.In(plus8).Hour() != 9 {
		t.Fatalf("local hour = %d; want 9", got.In(plus8).Hour())
	}

	// A negative offset crosses the date line the other way.
	minus5 := time.FixedZone("UTC-5", -5*3600)
	s5, err := ParseCron("0 9 * * *", minus5)
	if err != nil {
		t.Fatal(err)
	}
	got5 := s5.Next(utc(2026, 3, 5, 12, 0)) // 07:00 local
	if want5 := utc(2026, 3, 5, 14, 0); !got5.Equal(want5) {
		t.Fatalf("09:00 UTC-5 = %s; want %s", got5.UTC().Format(time.RFC3339), want5.Format(time.RFC3339))
	}

	// scheduleZone: 0 is exactly UTC, anything else a fixed offset.
	if scheduleZone(0) != time.UTC {
		t.Fatal("a zero offset must be UTC itself")
	}
	if off := scheduleZone(8 * time.Hour); off.String() != "UTC+8" {
		t.Fatalf("zone name = %q", off.String())
	}
	if half := scheduleZone(-5*time.Hour - 30*time.Minute); func() int {
		_, secs := time.Date(2026, 1, 1, 0, 0, 0, 0, half).Zone()
		return secs
	}() != -19800 {
		t.Fatal("half-hour offsets must survive")
	}
}

// TestParseCronNeverOccurs: "Feb 30" has no future occurrence — the search
// stops instead of spinning, and the zero time tells the caller so.
func TestParseCronNeverOccurs(t *testing.T) {
	s, err := ParseCron("0 0 31 2 *", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Next(utc(2026, 3, 5, 0, 0)); !got.IsZero() {
		t.Fatalf("expected no occurrence, got %s", got)
	}
}

// TestParseCronErrors: malformed expressions are rejected with a message that
// names the field.
func TestParseCronErrors(t *testing.T) {
	bad := []string{
		"",                        // no fields
		"* * * *",                 // 4 fields
		"* * * * * *",             // 6 fields
		"60 * * * *",              // minute out of range
		"* 24 * * *",              // hour out of range
		"* * 0 * *",               // day-of-month out of range
		"* * * 13 *",              // month out of range
		"* * * * 8",               // day-of-week out of range
		"1-0 * * * *",             // inverted range
		"a * * * *",               // not a number
		"*/0 * * * *",             // zero step
		"*/-1 * * * *",            // negative step
		"1,,2 * * * *",            // empty list item
		"1- * * * *",              // missing range end
		"* * * * mon",             // names are not supported
		"0 9 * * MON-FRI",         // ... at all
		"* * * * * * * * * * * *", // wildly wrong
	}
	for _, expr := range bad {
		if _, err := ParseCron(expr, time.UTC); err == nil {
			t.Errorf("ParseCron(%q) must fail", expr)
		}
	}
}
