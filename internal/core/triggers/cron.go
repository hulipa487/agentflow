package triggers

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Cron expression support for engine triggers: the classic 5 fields
// (minute hour day-of-month month day-of-week) with *, N, N-M, */N, N-M/N, and
// comma-separated lists. It is a strict superset of what scheduler.cron offers
// loops (which keeps its own documented subset) — the engine path is the one
// deployments describe schedules in, so it takes the full form.
//
// Evaluation happens in a fixed UTC offset (runtime.timezone_offset_hours),
// never in a named zone: there is no daylight saving to shift a schedule.

// field is one parsed cron field: its allowed values, ascending.
type field struct {
	values []int
	star   bool // the field was a bare * (drives the day-of-month/day-of-week OR rule)
}

func (f field) has(v int) bool {
	for _, x := range f.values {
		if x == v {
			return true
		}
	}
	return false
}

// Schedule is a parsed cron expression bound to a fixed zone.
type Schedule struct {
	expr                    string
	minute, hour            field
	dom, month, dow         field
	loc                     *time.Location
	domRestricted, dowRestr bool
}

// ParseCron parses a 5-field cron expression. loc is the zone the fields are
// matched in (see scheduleZone); nil means UTC.
func ParseCron(expr string, loc *time.Location) (*Schedule, error) {
	if loc == nil {
		loc = time.UTC
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q: expected 5 fields (minute hour day-of-month month day-of-week), got %d", expr, len(fields))
	}
	minute, err := parseField(fields[0], "minute", 0, 59, nil)
	if err != nil {
		return nil, fmt.Errorf("cron %q: %w", expr, err)
	}
	hour, err := parseField(fields[1], "hour", 0, 23, nil)
	if err != nil {
		return nil, fmt.Errorf("cron %q: %w", expr, err)
	}
	dom, err := parseField(fields[2], "day-of-month", 1, 31, nil)
	if err != nil {
		return nil, fmt.Errorf("cron %q: %w", expr, err)
	}
	month, err := parseField(fields[3], "month", 1, 12, nil)
	if err != nil {
		return nil, fmt.Errorf("cron %q: %w", expr, err)
	}
	// Day-of-week accepts 0 and 7 for Sunday.
	dow, err := parseField(fields[4], "day-of-week", 0, 7, func(v int) int {
		if v == 7 {
			return 0
		}
		return v
	})
	if err != nil {
		return nil, fmt.Errorf("cron %q: %w", expr, err)
	}
	return &Schedule{
		expr: expr, minute: minute, hour: hour, dom: dom, month: month, dow: dow,
		loc: loc, domRestricted: !dom.star, dowRestr: !dow.star,
	}, nil
}

// Expression returns the source expression (for logs).
func (s *Schedule) Expression() string { return s.expr }

// cronHorizonDays bounds the search for the next occurrence. A day-of-month
// that never occurs (Feb 30) stops the search instead of spinning forever.
const cronHorizonDays = 366 * 5

// Next returns the first occurrence strictly after the minute containing
// `after` — cron has minute resolution. The zero time means "never".
func (s *Schedule) Next(after time.Time) time.Time {
	t := after.In(s.loc)
	start := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, s.loc).Add(time.Minute)
	for d := 0; d < cronHorizonDays; d++ {
		day := start.AddDate(0, 0, d)
		if !s.month.has(int(day.Month())) || !s.dayMatches(day) {
			continue
		}
		for _, h := range s.hour.values {
			for _, m := range s.minute.values {
				cand := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, s.loc)
				if !cand.Before(start) {
					return cand
				}
			}
		}
	}
	return time.Time{}
}

// dayMatches applies the classic cron rule: when both day-of-month and
// day-of-week are restricted, a day matching either one is selected; when only
// one is restricted, that one decides. A bare * on both matches every day.
func (s *Schedule) dayMatches(t time.Time) bool {
	dom := s.dom.has(t.Day())
	dow := s.dow.has(int(t.Weekday()))
	switch {
	case s.domRestricted && s.dowRestr:
		return dom || dow
	case s.domRestricted:
		return dom
	case s.dowRestr:
		return dow
	default:
		return true
	}
}

// parseField parses one field: "*", "N", "N-M", "*/N", "N-M/N", "N/N", or a
// comma-separated list of those.
func parseField(raw, name string, min, max int, normalize func(int) int) (field, error) {
	out := field{star: strings.TrimSpace(raw) == "*"}
	seen := map[int]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return field{}, fmt.Errorf("%s: empty list item", name)
		}
		step := 1
		base := part
		if b, s, ok := strings.Cut(part, "/"); ok {
			base = strings.TrimSpace(b)
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil || n <= 0 {
				return field{}, fmt.Errorf("%s: %q is not a positive step", name, part)
			}
			step = n
		}
		lo, hi := min, max
		switch {
		case base == "*":
			// whole range
		case strings.Contains(base, "-"):
			a, b, _ := strings.Cut(base, "-")
			var err error
			if lo, err = strconv.Atoi(strings.TrimSpace(a)); err != nil {
				return field{}, fmt.Errorf("%s: %q is not a number", name, a)
			}
			if hi, err = strconv.Atoi(strings.TrimSpace(b)); err != nil {
				return field{}, fmt.Errorf("%s: %q is not a number", name, b)
			}
			if lo > hi {
				return field{}, fmt.Errorf("%s: range %d-%d is inverted", name, lo, hi)
			}
		default:
			v, err := strconv.Atoi(base)
			if err != nil {
				return field{}, fmt.Errorf("%s: %q is not a number, a range, or *", name, base)
			}
			lo, hi = v, v
			if step > 1 {
				hi = max // "N/step" means N..max step step
			}
		}
		if lo < min || hi > max {
			return field{}, fmt.Errorf("%s: %d-%d is outside %d..%d", name, lo, hi, min, max)
		}
		for v := lo; v <= hi; v += step {
			n := v
			if normalize != nil {
				n = normalize(n)
			}
			if !seen[n] {
				seen[n] = true
				out.values = append(out.values, n)
			}
		}
	}
	if len(out.values) == 0 {
		return field{}, fmt.Errorf("%s: no values", name)
	}
	sort.Ints(out.values)
	return out, nil
}

