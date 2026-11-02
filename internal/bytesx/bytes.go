// Package bytesx parses and formats byte sizes as operators write them
// ("500MB", "5GiB"). Single-letter suffixes are binary: k means KiB.
package bytesx

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

var units = map[string]int64{
	"":    1,
	"b":   1,
	"k":   1 << 10,
	"kb":  1 << 10,
	"kib": 1 << 10,
	"m":   1 << 20,
	"mb":  1 << 20,
	"mib": 1 << 20,
	"g":   1 << 30,
	"gb":  1 << 30,
	"gib": 1 << 30,
	"t":   1 << 40,
	"tb":  1 << 40,
	"tib": 1 << 40,
}

// Parse converts a human size into a byte count. A plain number is bytes.
// Negative sizes and unknown suffixes are rejected.
func Parse(s string) (int64, error) {
	orig := s
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}

	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	numPart, unitPart := s[:i], strings.TrimSpace(s[i:])
	if numPart == "" {
		return 0, fmt.Errorf("invalid size %q: expected a number", orig)
	}

	n, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", orig, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid size %q: must not be negative", orig)
	}

	mult, ok := units[unitPart]
	if !ok {
		return 0, fmt.Errorf("invalid size %q: unknown unit %q", orig, unitPart)
	}

	value := n * float64(mult)
	if value > math.MaxInt64 {
		return 0, fmt.Errorf("invalid size %q: too large", orig)
	}
	return int64(value), nil
}

// Format renders a byte count with one decimal in the largest exact binary
// unit that keeps the number readable.
func Format(b int64) string {
	if b < 0 {
		return "-" + Format(-b)
	}
	const step = 1024
	if b < step {
		return fmt.Sprintf("%d B", b)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(b)
	i := -1
	for value >= step && i < len(units)-1 {
		value /= step
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}
