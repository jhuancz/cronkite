// Package lock prevents overlapping runs.
//
// Two cronkite processes deleting the same tree is the classic way to produce
// phantom failures: the second run reports files that "vanished", the alert
// fires, and an operator spends an hour on a bug that does not exist. The lock
// makes that impossible, and the "locked" exit code tells cron this was not a
// failure.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// ErrHeld means another run is in progress.
var ErrHeld = errors.New("another cronkite run holds the lock")

// Handle is an acquired lock.
type Handle struct {
	path string
	file *flock.Flock
}

// Acquire takes an exclusive, non-blocking lock at path. It never waits: a
// scheduled run that finds the lock taken must exit quickly, not pile up.
func Acquire(path string) (*Handle, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create lock directory: %w", err)
		}
	}

	f := flock.New(path)
	ok, err := f.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire lock %s: %w", path, err)
	}
	if !ok {
		return nil, ErrHeld
	}
	return &Handle{path: path, file: f}, nil
}

// Release drops the lock. It is safe to call once.
func (h *Handle) Release() error {
	if h == nil || h.file == nil {
		return nil
	}
	err := h.file.Unlock()
	h.file = nil
	return err
}

// Path is the lock file location, used in reports and diagnostics.
func (h *Handle) Path() string { return h.path }
