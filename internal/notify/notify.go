// Package notify tells an operator what a run did.
//
// One message per run, not per task: the report already aggregates, and an alert
// per failed file trains an operator to mute the channel. Three rules shape this
// package:
//   - A notifier cannot change a run. A Telegram outage, a wrong chat id or an
//     expired token becomes a note in the report; the exit code keeps describing
//     the maintenance, not the messenger.
//   - A message never carries the token. The token travels in the request URL
//     and the HTTP client quotes that URL in its errors, so every error leaving
//     this package is sanitized first.
//   - Nothing is sent in a rehearsal. A dry run must not let an operator
//     discover that their bot posted to a team channel.
package notify

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jhuancz/cronkite/internal/bytesx"
	"github.com/jhuancz/cronkite/internal/report"
)

// Notifier delivers one message about a finished run. Telegram is one
// implementation; an SMTP sender can be another without touching the engine.
type Notifier interface {
	Send(ctx context.Context, m Message) error
}

// Message is what an operator needs to decide whether to open the report: the
// outcome, which run and which machine produced it, the counts that were
// changed, the tasks that failed, and where the report is.
//
// The report path is part of the message on purpose: it is the answer to "where
// do I look", and it is only knowable after the report has been written, which
// is why delivery happens last.
type Message struct {
	Status      report.Status
	RunID       string
	Host        string
	Totals      report.Totals
	FailedTasks []string
	ReportPath  string
}

// Text renders the message body once, so every implementation sends the same
// words and a test can assert them without a transport.
//
// Only buckets that happened are named: a run that wrote nothing says nothing
// about writing, because "0 files written" reads like a failed backup rather
// than an idle cleanup.
func (m Message) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cronkite run %s", m.Status)
	if m.Host != "" {
		fmt.Fprintf(&b, " on %s", m.Host)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "run: %s\n", m.RunID)
	if n := m.Totals.FilesRemoved; n > 0 {
		fmt.Fprintf(&b, "removed: %d files, %s\n", n, bytesx.Format(m.Totals.BytesFreed))
	}
	if n := m.Totals.FilesWritten + m.Totals.FilesRelinked; n > 0 {
		fmt.Fprintf(&b, "written: %d files, %s\n", n,
			bytesx.Format(m.Totals.BytesWritten+m.Totals.BytesRelinked))
	}
	fmt.Fprintf(&b, "skipped: %d, errors: %d\n", m.Totals.Skipped, m.Totals.Errors)
	if len(m.FailedTasks) > 0 {
		fmt.Fprintf(&b, "failed tasks: %s\n", strings.Join(m.FailedTasks, ", "))
	}
	fmt.Fprintf(&b, "report: %s", m.ReportPath)
	return b.String()
}

// Severity orders run outcomes. The order is explicit and fixed:
//
//	ok < partial < failed
//
// It exists so the throttle can express "a worse outcome is never suppressed" in
// code instead of in prose. Two mappings are deliberate:
//   - refused ranks with failed: exit code 4 means a human has to look at the
//     configuration, which is never less urgent than a failed task.
//   - skipped ranks with ok: a locked run means another run overlapped, which is
//     expected on a busy machine and must not read as a failure.
type Severity int

const (
	// SeverityOK is a run that changed things without reporting failures, or
	// that did nothing at all.
	SeverityOK Severity = iota
	// SeverityPartial is a run that reported failures while still finishing.
	SeverityPartial
	// SeverityFailed is a run that failed outright or was refused by a ceiling.
	SeverityFailed
)

// SeverityOf maps a run status onto the fixed order above.
func SeverityOf(status report.Status) Severity {
	switch status {
	case report.StatusPartial:
		return SeverityPartial
	case report.StatusFailed, report.StatusRefused:
		return SeverityFailed
	default:
		// ok and skipped. An unknown status added later also lands here, so a new
		// value cannot silently become an alert-worthy failure.
		return SeverityOK
	}
}

// Filter is the event filter built from notify.on.
type Filter struct {
	never  bool
	always bool
	failed bool
}

// NewFilter builds the filter from the configured event names.
//
// "never" wins over everything else, including "always": it is the switch an
// operator flips to stop the alerts, and a list that still sends would be a
// configuration trap. An empty or unknown list matches nothing; the
// configuration supplies the default and rejects unknown names, so this only
// matters to a caller that skipped both.
func NewFilter(on []string) Filter {
	var f Filter
	for _, event := range on {
		switch event {
		case "never":
			f.never = true
		case "always":
			f.always = true
		case "failure", "partial":
			f.failed = true
		}
	}
	return f
}

// Allows reports whether a run that ended with status is worth a message.
//
// "failure" and "partial" share one threshold on purpose. cronkite's run status
// is "partial" exactly when at least one task reported failures while the run
// finished, so "failure" - the name an operator writes to mean "tell me when
// something breaks" - has to cover it, and "partial" cannot exclude a complete
// failure, which is the same news only worse. The default configuration names
// both, which is why the two events describing one threshold is not a gap.
func (f Filter) Allows(status report.Status) bool {
	if f.never {
		return false
	}
	if f.always {
		return true
	}
	return f.failed && SeverityOf(status) >= SeverityPartial
}

// StateFileName is the throttle state file inside the run state directory. It is
// fixed because two processes - this run and the next one - read the same file.
const StateFileName = "notify-state.json"

// StatePath returns the throttle state file for a state directory.
func StatePath(stateDir string) string {
	return filepath.Join(stateDir, StateFileName)
}
