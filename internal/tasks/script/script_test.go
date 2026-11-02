package script

import (
	"strings"
	"testing"
	"time"

	"github.com/jhuancz/cronkite/internal/report"
	"github.com/jhuancz/cronkite/internal/task"
)

var clock = time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)

// The script is told what kind of run it belongs to, in both modes, so it can
// branch on a rehearsal and can log the run it served. Plan mode runs nothing,
// so this is the only place the plan-mode value can be asserted.
func TestEnvironmentCarriesTheRunContract(t *testing.T) {
	script := &Task{name: "maint"}

	cases := []struct {
		mode      report.Mode
		wantMode  string
		wantDryRu string
	}{
		{mode: report.ModePlan, wantMode: "plan", wantDryRu: "1"},
		{mode: report.ModeApply, wantMode: "apply", wantDryRu: "0"},
	}
	for _, tc := range cases {
		env := script.environ(task.Env{Mode: tc.mode, Now: clock})
		got := map[string]string{}
		for _, entry := range env {
			key, value, ok := strings.Cut(entry, "=")
			if !ok {
				t.Fatalf("%q is not a KEY=VALUE pair", entry)
			}
			got[key] = value
		}

		if got["CRONKITE_MODE"] != tc.wantMode {
			t.Errorf("%s: CRONKITE_MODE = %q, want %q", tc.mode, got["CRONKITE_MODE"], tc.wantMode)
		}
		if got["CRONKITE_DRY_RUN"] != tc.wantDryRu {
			t.Errorf("%s: CRONKITE_DRY_RUN = %q, want %q", tc.mode, got["CRONKITE_DRY_RUN"], tc.wantDryRu)
		}
		if got["CRONKITE_TASK"] != "maint" {
			t.Errorf("%s: CRONKITE_TASK = %q, want %q", tc.mode, got["CRONKITE_TASK"], "maint")
		}
		if want := "20260504T120000Z"; got["CRONKITE_RUN_ID"] != want {
			t.Errorf("%s: CRONKITE_RUN_ID = %q, want %q", tc.mode, got["CRONKITE_RUN_ID"], want)
		}
		if len(env) != 4 {
			t.Errorf("%s: environment = %v, want exactly the four cronkite variables", tc.mode, env)
		}
	}
}

// The cap bounds what the report keeps and never what the script may write: a
// noisy run still finishes, and the note says how much was dropped.
func TestCaptureKeepsTheFirstBytesAndCountsTheRest(t *testing.T) {
	c := &capture{limit: 8}
	if _, err := c.Write([]byte("abcdef")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := c.Write([]byte("ghijklmnop")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got, want := c.String(), "abcdefgh"; got != want {
		t.Errorf("captured %q, want %q", got, want)
	}
	if !c.Truncated() {
		t.Error("Truncated() = false, want true once the cap was passed")
	}
	if got, want := c.total, 16; got != want {
		t.Errorf("total = %d, want %d", got, want)
	}

	small := &capture{limit: 8}
	if _, err := small.Write([]byte("abc")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if small.Truncated() {
		t.Error("Truncated() = true, want false for output under the cap")
	}
}
