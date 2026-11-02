package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhuancz/cronkite/internal/report"
)

// throttleAt returns a throttle whose clock the caller moves by hand, so the
// window is exercised without sleeping.
func throttleAt(t *testing.T, path string, window time.Duration, now *time.Time) *Throttle {
	t.Helper()
	return NewThrottle(path, window, func() time.Time { return *now })
}

func TestThrottleSuppressesARepeatInsideTheWindow(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), StateFileName)
	th := throttleAt(t, path, 15*time.Minute, &now)

	if th.Suppressed(report.StatusPartial) {
		t.Fatal("the first outcome cannot be a repeat")
	}
	if err := th.Record(report.StatusPartial); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !th.Suppressed(report.StatusPartial) {
		t.Fatal("an identical outcome inside the window must be suppressed")
	}

	now = now.Add(14 * time.Minute)
	if !th.Suppressed(report.StatusPartial) {
		t.Fatal("the window has not expired yet, the repeat must stay suppressed")
	}

	now = now.Add(2 * time.Minute)
	if th.Suppressed(report.StatusPartial) {
		t.Fatal("an outcome after the window must be sent again")
	}
}

// The frozen decision: silence during a new failure is the one thing an alert
// must not do.
func TestThrottleNeverSuppressesAnEscalation(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), StateFileName)
	th := throttleAt(t, path, 15*time.Minute, &now)

	for _, sent := range []report.Status{report.StatusOK, report.StatusPartial} {
		if err := th.Record(sent); err != nil {
			t.Fatalf("Record(%q): %v", sent, err)
		}
	}
	if th.Suppressed(report.StatusFailed) {
		t.Fatal("a failed run must be sent immediately even after a partial one")
	}
	if th.Suppressed(report.StatusRefused) {
		t.Fatal("a refused run deserves a human and must never be suppressed")
	}
}

func TestThrottleSendsAnImprovement(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), StateFileName)
	th := throttleAt(t, path, 15*time.Minute, &now)

	if err := th.Record(report.StatusFailed); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if th.Suppressed(report.StatusPartial) {
		t.Fatal("a different status is not a repeat, so it must be sent")
	}
}

// The state is shared by separate processes, so it has to be small, stable and
// on disk in the state directory.
func TestThrottleStateIsASmallJSONFile(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	path := StatePath(stateDir)
	if filepath.Base(path) != "notify-state.json" {
		t.Fatalf("state file = %q, want the documented name", filepath.Base(path))
	}
	th := throttleAt(t, path, 15*time.Minute, &now)
	if err := th.Record(report.StatusFailed); err != nil {
		t.Fatalf("Record: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("the state must be JSON: %v", err)
	}
	if len(state) != 2 {
		t.Fatalf("state = %s, want exactly status and sent_at", data)
	}
	var status report.Status
	if err := json.Unmarshal(state["status"], &status); err != nil || status != report.StatusFailed {
		t.Errorf("stored status = %q (%v), want failed", status, err)
	}
	var sentAt time.Time
	if err := json.Unmarshal(state["sent_at"], &sentAt); err != nil || !sentAt.Equal(now) {
		t.Errorf("stored sent_at = %s (%v), want %s", sentAt, err, now)
	}

	// A second process reads the same file and sees the same history.
	later := now.Add(time.Minute)
	if !throttleAt(t, path, 15*time.Minute, &later).Suppressed(report.StatusFailed) {
		t.Fatal("a fresh throttle must read what the previous run wrote")
	}
}

// A broken state directory may make cronkite repeat an alert, but it must never
// be able to lose one.
func TestThrottleReadsACorruptStateAsNothingSent(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), StateFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	th := throttleAt(t, path, 15*time.Minute, &now)
	if th.Suppressed(report.StatusFailed) {
		t.Fatal("a corrupt state file must not silence an alert")
	}
	if err := th.Record(report.StatusFailed); err != nil {
		t.Fatalf("Record must replace a corrupt state file: %v", err)
	}
	if !th.Suppressed(report.StatusFailed) {
		t.Fatal("the rewritten state must be readable")
	}
}

func TestThrottleWithoutAWindowNeverSuppresses(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), StateFileName)
	th := throttleAt(t, path, 0, &now)
	if err := th.Record(report.StatusFailed); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if th.Suppressed(report.StatusFailed) {
		t.Fatal("a zero window means no throttling at all")
	}
}
