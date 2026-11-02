package notify

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jhuancz/cronkite/internal/report"
)

// Throttle suppresses a repeat of an outcome inside notify.throttle.
//
// The memory is one small JSON file in the state directory, because the process
// that sent the last alert is usually gone by the time the next run starts.
type Throttle struct {
	path   string
	window time.Duration
	now    func() time.Time
}

// throttleState is the on-disk record: the last status that was delivered, and
// when. Nothing else is stored - a token, a chat id or a run id here would be a
// secret on disk or a record that grows without bound.
type throttleState struct {
	Status report.Status `json:"status"`
	SentAt time.Time     `json:"sent_at"`
}

// NewThrottle returns a throttle backed by path. A nil clock means time.Now;
// tests move the clock instead of sleeping.
func NewThrottle(path string, window time.Duration, now func() time.Time) *Throttle {
	if now == nil {
		now = time.Now
	}
	return &Throttle{path: path, window: window, now: now}
}

// Suppressed reports whether a message for status would only repeat what the
// operator already received inside the window.
//
// A worse status is never suppressed: silence during a new failure is the one
// thing an alert must not do. That is why the comparison uses the explicit order
// ok < partial < failed and not a timer alone.
//
// An unreadable or corrupt state file reads as "nothing was sent", so a broken
// state directory can make cronkite repeat an alert but never lose one.
func (t *Throttle) Suppressed(status report.Status) bool {
	state, err := readThrottleState(t.path)
	if err != nil {
		return false
	}
	if SeverityOf(status) > SeverityOf(state.Status) {
		return false
	}
	if state.Status != status {
		return false
	}
	if t.window <= 0 {
		return false
	}
	return t.now().UTC().Sub(state.SentAt) < t.window
}

// Record stores status and the current time as the last delivered message.
//
// Only a successful delivery is recorded: a failed send must not start a window
// that would silence the retry on the next run.
func (t *Throttle) Record(status report.Status) error {
	if dir := filepath.Dir(t.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create notify state directory: %w", err)
		}
	}
	return writeThrottleState(t.path, throttleState{Status: status, SentAt: t.now().UTC()})
}

// readThrottleState decodes the state file. A missing file is an error too: the
// caller treats every error as "nothing was sent", so the distinction only
// matters to a test.
func readThrottleState(path string) (throttleState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return throttleState{}, fmt.Errorf("read notify state %s: %w", path, err)
	}
	var state throttleState
	if err := json.Unmarshal(data, &state); err != nil {
		return throttleState{}, fmt.Errorf("decode notify state %s: %w", path, err)
	}
	return state, nil
}

// writeThrottleState publishes the state atomically, so a run starting at the
// same moment never reads a half-written file and concludes nothing was sent.
func writeThrottleState(path string, state throttleState) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".notify-*.json")
	if err != nil {
		return fmt.Errorf("create notify state next to %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(state); err != nil {
		tmp.Close()
		return fmt.Errorf("write notify state %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write notify state %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("publish notify state %s: %w", path, err)
	}
	return nil
}
