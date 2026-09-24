package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseEvery parses an every: interval: a duration in ms, s, m, h, or d units,
// optionally compound ("90s", "15m", "6h", "1d", "2d12h") with fractional
// values allowed ("1.5h"). Go's other duration units are not accepted — the
// smallest interval the trigger service schedules is minEvery, so an odd unit is
// a typo worth naming rather than silently honouring.
//
// It lives in config, not in the trigger package that first needed it, because
// the unit vocabulary is config syntax: triggers, a memory store's retention and
// anything else written as a deployment duration all share it. The trigger
// package imports config, so the direction is the one that already existed.
func ParseEvery(s string) (time.Duration, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return 0, fmt.Errorf("empty interval")
	}
	var total time.Duration
	for i := 0; i < len(in); {
		j := i
		for j < len(in) && (in[j] >= '0' && in[j] <= '9' || in[j] == '.') {
			j++
		}
		if j == i {
			return 0, fmt.Errorf("%q: expected a number", in[i:])
		}
		if j >= len(in) {
			return 0, fmt.Errorf("%q: missing unit (use ms, s, m, h, or d)", in)
		}
		value, err := strconv.ParseFloat(in[i:j], 64)
		if err != nil {
			return 0, fmt.Errorf("%q: bad number %q", in, in[i:j])
		}
		var unit time.Duration
		next := j + 1
		switch in[j] {
		case 's':
			unit = time.Second
		case 'm':
			// "ms" is milliseconds; a bare "m" is minutes.
			if j+1 < len(in) && in[j+1] == 's' {
				unit, next = time.Millisecond, j+2
			} else {
				unit = time.Minute
			}
		case 'h':
			unit = time.Hour
		case 'd':
			unit = 24 * time.Hour
		default:
			return 0, fmt.Errorf("%q: unknown unit %q (use ms, s, m, h, or d)", in, string(in[j]))
		}
		total += time.Duration(value * float64(unit))
		i = next
	}
	if total <= 0 {
		return 0, fmt.Errorf("%q: interval must be positive", in)
	}
	return total, nil
}

// ParseRetention parses a memory store's retention: the same units an every:
// interval takes, or the literal "forever" for a record that never expires.
//
// It exists because main parsed this with time.ParseDuration and discarded the
// error, and Go's parser has no day unit — so the shipped `retention: "30d"`
// silently became zero, which reads as "never expires". The field looked
// configured and did nothing.
func ParseRetention(s string) (time.Duration, error) {
	if strings.EqualFold(strings.TrimSpace(s), "forever") {
		return 0, nil
	}
	return ParseEvery(s)
}
