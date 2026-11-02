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
	path    string
	window  time.Duration
	now     func() time.Time
	channel string
}

// throttleState is the on-disk record, keyed by channel: the last status that was
// delivered to that channel, and when.
//
// Keyed by channel and not by state directory because one state directory can
// describe more than one destination - two chats, a chat and a webhook - and a
// window shared between them would silence an alert nobody received. Nothing else
// is stored: a token or a run id here would be a secret on disk or a record that
// grows without bound.
type throttleState struct {
	Channels map[string]channelRecord `json:"channels"`
}

// channelRecord is what was last delivered to one channel.
type channelRecord struct {
	Status report.Status `json:"status"`
	SentAt time.Time     `json:"sent_at"`
}

// NewThrottle returns a throttle backed by path. A nil clock means time.Now;
// tests move the clock instead of sleeping.
func NewThrottle(path string, window time.Duration, channel string, now func() time.Time) *Throttle {
	if now == nil {
		now = time.Now
	}
	return &Throttle{path: path, window: window, now: now, channel: channel}
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
	last, ok := state.Channels[t.channel]
	if !ok {
		// Nothing has been delivered to this channel yet, whatever was delivered
		// to the others in the same state directory.
		return false
	}
	if SeverityOf(status) > SeverityOf(last.Status) {
		return false
	}
	if last.Status != status {
		return false
	}
	if t.window <= 0 {
		return false
	}
	return t.now().UTC().Sub(last.SentAt) < t.window
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
	// Read-modify-write: recording one channel must not forget the others.
	state, err := readThrottleState(t.path)
	if err != nil {
		state = throttleState{}
	}
	if state.Channels == nil {
		state.Channels = map[string]channelRecord{}
	}
	state.Channels[t.channel] = channelRecord{Status: status, SentAt: t.now().UTC()}
	return writeThrottleState(t.path, state)
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
