// Package durationx parses retention windows. It accepts every unit that
// time.ParseDuration understands plus days and weeks, because operators write
// "30d", not "720h".
package durationx

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Parse converts a duration string such as "7d", "30d", "2w" or "1h30m".
func Parse(s string) (time.Duration, error) {
	orig := s
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}

	neg := false
	if c := s[0]; c == '-' || c == '+' {
		neg = c == '-'
		s = s[1:]
	}
	if s == "" {
		return 0, fmt.Errorf("invalid duration %q", orig)
	}

	var total time.Duration
	for len(s) > 0 {
		i := 0
		for i < len(s) && (isDigit(s[i]) || s[i] == '.') {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("invalid duration %q: expected a number at %q", orig, s)
		}
		number, err := strconv.ParseFloat(s[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: bad number %q", orig, s[:i])
		}
		s = s[i:]

		j := 0
		for j < len(s) && !isDigit(s[j]) && s[j] != '.' {
			j++
		}
		unit := s[:j]
		s = s[j:]

		mult, err := unitDuration(unit)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", orig, err)
		}
		total += time.Duration(number * float64(mult))
	}

	if neg {
		total = -total
	}
	return total, nil
}

func unitDuration(unit string) (time.Duration, error) {
	switch unit {
	case "d", "day", "days":
		return 24 * time.Hour, nil
	case "w", "week", "weeks":
		return 7 * 24 * time.Hour, nil
	case "":
		return 0, fmt.Errorf("missing unit (want ns, us, ms, s, m, h, d or w)")
	}
	d, err := time.ParseDuration("1" + unit)
	if err != nil {
		return 0, fmt.Errorf("unknown unit %q", unit)
	}
	return d, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
