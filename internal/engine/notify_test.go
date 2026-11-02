package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhuancz/cronkite/internal/config"
	"github.com/jhuancz/cronkite/internal/exitcode"
	"github.com/jhuancz/cronkite/internal/notify"
	"github.com/jhuancz/cronkite/internal/report"
)

// spyNotifier records every message the engine would deliver, so a test can
// assert delivery, its content and its side effects without a network.
type spyNotifier struct {
	messages []notify.Message
	err      error
}

func (s *spyNotifier) Send(_ context.Context, m notify.Message) error {
	s.messages = append(s.messages, m)
	return s.err
}

// notifyOnFailure is the configuration the delivery tests start from: alerts on
// any failure, the default window, one chat.
const notifyOnFailure = `
  enabled: true
  on: [failure]
  throttle: "15m"
  telegram:
    chat_id: "42"
`

// notifyFixture is one configuration that can end three ways: the backup task
// cannot read its snapshot index (partial), the cleanup task has more work than
// its safety ceiling allows (refused), and forcing that ceiling gives a clean run
// (ok). The state directory is shared, so the throttle sees a history.
type notifyFixture struct {
	cfg       *config.Config
	reportDir string
	stateDir  string
}

func newNotifyFixture(t *testing.T, notifyYAML string) *notifyFixture {
	t.Helper()
	stateDir := t.TempDir()
	reportDir := filepath.Join(t.TempDir(), "reports")

	logs := t.TempDir()
	oldFile(t, logs, "a.log")
	oldFile(t, logs, "b.log")

	src := backupSource(t)
	dest := filepath.Join(t.TempDir(), "snapshots")
	// An unreadable snapshot index makes the plan fail, which is a task failure
	// rather than a refusal and needs no privileged filesystem tricks.
	snapshot := filepath.Join(dest, "20260101T000000Z")
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "manifest.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := newConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
  safety:
    max_delete_files: 1
report:
  dir: %s
notify:
%s
tasks:
  - name: data
    type: backup
    roots: [%s]
    destination: %s
  - name: logs
    type: logclean
    roots: [%s]
    older_than: 30d
`, stateDir, reportDir, notifyYAML, src, dest, logs))
	return &notifyFixture{cfg: cfg, reportDir: reportDir, stateDir: stateDir}
}

// run executes the named tasks against the fixture configuration.
func (f *notifyFixture) run(t *testing.T, only string, opts Options) *report.Report {
	t.Helper()
	opts.Only = []string{only}
	opts.Now = fixedNow
	rep, err := Run(context.Background(), f.cfg, opts)
	if err != nil {
		t.Fatalf("Run(%s): %v", only, err)
	}
	return rep
}

// A failing run sends exactly one message, and the message names the outcome,
// the failed task and a report that exists.
func TestNotifySendsOneMessageForAFailingRun(t *testing.T) {
	f := newNotifyFixture(t, notifyOnFailure)
	spy := &spyNotifier{}

	rep := f.run(t, "data", Options{Apply: true, Notifier: spy})

	if rep.Status != report.StatusPartial || rep.ExitCode != exitcode.Partial {
		t.Fatalf("status = %q exit = %d, want partial/2", rep.Status, rep.ExitCode)
	}
	if len(spy.messages) != 1 {
		t.Fatalf("sent %d messages, want exactly one per run", len(spy.messages))
	}
	msg := spy.messages[0]
	if msg.Status != rep.Status {
		t.Errorf("message status = %q, want %q", msg.Status, rep.Status)
	}
	if msg.RunID != rep.RunID {
		t.Errorf("message run id = %q, want %q", msg.RunID, rep.RunID)
	}
	if msg.Host != rep.Host.Hostname {
		t.Errorf("message host = %q, want %q", msg.Host, rep.Host.Hostname)
	}
	if got := strings.Join(msg.FailedTasks, ","); got != "data" {
		t.Errorf("failed tasks = %q, want data", got)
	}
	wantPath := filepath.Join(f.reportDir, "run-"+rep.RunID+".json")
	if msg.ReportPath != wantPath {
		t.Errorf("report path = %q, want %q", msg.ReportPath, wantPath)
	}
	if _, err := os.Stat(msg.ReportPath); err != nil {
		t.Errorf("the message must reference a report that exists: %v", err)
	}
	text := msg.Text()
	for _, want := range []string{"partial", rep.RunID, "data", wantPath} {
		if !strings.Contains(text, want) {
			t.Errorf("the message should mention %q, got:\n%s", want, text)
		}
	}
}

// "never" is the switch an operator flips to stop the alerts: nothing may leave
// the machine, whatever the outcome.
func TestNotifyOnNeverSendsNothing(t *testing.T) {
	f := newNotifyFixture(t, `
  enabled: true
  on: [never]
  telegram:
    chat_id: "42"
`)
	spy := &spyNotifier{}

	rep := f.run(t, "data", Options{Apply: true, Notifier: spy})

	if rep.Status != report.StatusPartial {
		t.Fatalf("status = %q, want partial", rep.Status)
	}
	if len(spy.messages) != 0 {
		t.Fatalf("sent %d messages with on: [never]", len(spy.messages))
	}
	for _, note := range rep.Notes {
		if strings.Contains(note, "notify:") {
			t.Errorf("on: [never] must not even try to reach the sender, note: %q", note)
		}
	}
}

func TestNotifyDisabledSendsNothing(t *testing.T) {
	f := newNotifyFixture(t, `
  enabled: false
  on: [always]
  telegram:
    chat_id: "42"
`)
	spy := &spyNotifier{}

	f.run(t, "data", Options{Apply: true, Notifier: spy})

	if len(spy.messages) != 0 {
		t.Fatalf("sent %d messages with notify.enabled false", len(spy.messages))
	}
}

// A rehearsal is a rehearsal: no message, and no throttle state left behind as
// evidence of one.
func TestPlanNeverNotifies(t *testing.T) {
	f := newNotifyFixture(t, `
  enabled: true
  on: [always]
  telegram:
    chat_id: "42"
`)
	spy := &spyNotifier{}

	rep := f.run(t, "data", Options{Apply: false, Notifier: spy})

	if rep.Mode != report.ModePlan {
		t.Fatalf("mode = %q, want plan", rep.Mode)
	}
	if len(spy.messages) != 0 {
		t.Fatalf("a rehearsal sent %d messages", len(spy.messages))
	}
	if _, err := os.Stat(notify.StatePath(f.stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a rehearsal must not write notify state, stat err = %v", err)
	}
}

// The frozen decision: a messenger failure is a note in the report, never a
// different exit code. The note has to survive in the written report, which is
// what an operator reads afterwards.
func TestNotifierFailureLeavesTheRunUntouched(t *testing.T) {
	f := newNotifyFixture(t, notifyOnFailure)
	spy := &spyNotifier{err: errors.New("telegram is unavailable (HTTP 500)")}

	rep := f.run(t, "data", Options{Apply: true, Notifier: spy})

	if rep.Status != report.StatusPartial {
		t.Errorf("status = %q, want the maintenance outcome partial", rep.Status)
	}
	if rep.ExitCode != exitcode.Partial {
		t.Errorf("exit code = %d, want the maintenance outcome %d", rep.ExitCode, exitcode.Partial)
	}
	notes := strings.Join(rep.Notes, "\n")
	if !strings.Contains(notes, "notify:") || !strings.Contains(notes, "HTTP 500") {
		t.Fatalf("the delivery failure should be a note, got:\n%s", notes)
	}

	written, err := report.ReadFile(filepath.Join(f.reportDir, "run-"+rep.RunID+".json"))
	if err != nil {
		t.Fatalf("read written report: %v", err)
	}
	if got := strings.Join(written.Notes, "\n"); !strings.Contains(got, "HTTP 500") {
		t.Errorf("the written report should carry the delivery note, got:\n%s", got)
	}
	if written.ExitCode != exitcode.Partial {
		t.Errorf("written exit code = %d, want %d", written.ExitCode, exitcode.Partial)
	}
}

// A repeat inside the window is suppressed; an escalation is not, even one
// second later, because silence during a new failure is the one thing an alert
// must not do.
func TestNotifyRepeatIsSuppressedAndEscalationIsNot(t *testing.T) {
	f := newNotifyFixture(t, notifyOnFailure)
	spy := &spyNotifier{}

	f.run(t, "data", Options{Apply: true, Notifier: spy})
	if len(spy.messages) != 1 {
		t.Fatalf("sent %d messages for the first failure, want 1", len(spy.messages))
	}

	repeat := f.run(t, "data", Options{Apply: true, Notifier: spy})
	if len(spy.messages) != 1 {
		t.Fatalf("sent %d messages, want the repeat suppressed", len(spy.messages))
	}
	if notes := strings.Join(repeat.Notes, "\n"); !strings.Contains(notes, "suppressed") {
		t.Errorf("a suppressed alert should say so, got:\n%s", notes)
	}

	// The refused run is worse than the partial one, so it goes out immediately.
	escalated := f.run(t, "logs", Options{Apply: true, Notifier: spy})
	if escalated.Status != report.StatusRefused {
		t.Fatalf("status = %q, want refused", escalated.Status)
	}
	if len(spy.messages) != 2 {
		t.Fatalf("sent %d messages, want the escalation delivered", len(spy.messages))
	}
	if got := spy.messages[1].Status; got != report.StatusRefused {
		t.Errorf("escalated message status = %q, want refused", got)
	}

	f.run(t, "logs", Options{Apply: true, Notifier: spy})
	if len(spy.messages) != 2 {
		t.Fatalf("sent %d messages, want the repeated refusal suppressed too", len(spy.messages))
	}

	if _, err := os.Stat(notify.StatePath(f.stateDir)); err != nil {
		t.Errorf("delivery should record its state in the state directory: %v", err)
	}
}

// on: [always] reaches a clean run, which is the only way an operator hears that
// maintenance is healthy again.
func TestNotifyAlwaysSendsForACleanRun(t *testing.T) {
	f := newNotifyFixture(t, `
  enabled: true
  on: [always]
  telegram:
    chat_id: "42"
`)
	spy := &spyNotifier{}

	rep := f.run(t, "logs", Options{Apply: true, Force: true, Notifier: spy})

	if rep.Status != report.StatusOK {
		t.Fatalf("status = %q, want ok", rep.Status)
	}
	if len(spy.messages) != 1 {
		t.Fatalf("sent %d messages, want one for a clean run with on: [always]", len(spy.messages))
	}
	if spy.messages[0].Status != report.StatusOK {
		t.Errorf("message status = %q, want ok", spy.messages[0].Status)
	}
}

// An enabled notifier with no token must stay silent when the outcome calls for
// silence: the token is only read when a message would actually be sent.
func TestNotifySilentOutcomeNeedsNoToken(t *testing.T) {
	f := newNotifyFixture(t, `
  enabled: true
  on: [failure]
  telegram:
    chat_id: "42"
    token_env: CRONKITE_TEST_ABSENT_TOKEN
`)
	t.Setenv("CRONKITE_TEST_ABSENT_TOKEN", "")

	// The cleanup run is forced clean, so nothing should be sent even though the
	// configured token is missing.
	rep := f.run(t, "logs", Options{Apply: true, Force: true})

	if rep.Status != report.StatusOK {
		t.Fatalf("status = %q, want ok", rep.Status)
	}
	for _, note := range rep.Notes {
		if strings.Contains(note, "token") {
			t.Errorf("a clean run must not read the token, note: %q", note)
		}
	}
}

// Guard against a fixture that stops failing: without a failing task the tests
// above would pass for the wrong reason.
func TestNotifyFixtureProducesTheIntendedStatuses(t *testing.T) {
	f := newNotifyFixture(t, notifyOnFailure)
	spy := &spyNotifier{}

	if got := f.run(t, "data", Options{Apply: true, Notifier: spy}).Status; got != report.StatusPartial {
		t.Errorf("data run status = %q, want partial", got)
	}
	spy = &spyNotifier{}
	if got := f.run(t, "logs", Options{Apply: true, Notifier: spy}).Status; got != report.StatusRefused {
		t.Errorf("logs run status = %q, want refused", got)
	}
	spy = &spyNotifier{}
	if got := f.run(t, "logs", Options{Apply: true, Force: true, Notifier: spy}).Status; got != report.StatusOK {
		t.Errorf("forced logs run status = %q, want ok", got)
	}
}
