package durationx

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	valid := []struct {
		in   string
		want time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"1.5d", 36 * time.Hour},
		{"90s", 90 * time.Second},
		{"-1d", -24 * time.Hour},
		{"0d", 0},
	}
	for _, tc := range valid {
		got, err := Parse(tc.in)
		if err != nil {
			t.Errorf("Parse(%q) returned error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{"", "  ", "30", "d", "30x", "abc", "1d2", "1//"} {
		if got, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", in, got)
		}
	}
}
