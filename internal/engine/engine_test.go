package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhuancz/cronkite/internal/config"
	"github.com/jhuancz/cronkite/internal/exitcode"
	"github.com/jhuancz/cronkite/internal/lock"
	"github.com/jhuancz/cronkite/internal/report"
)

// fixedNow keeps age comparisons deterministic.
var fixedNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func oldFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := fixedNow.Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

func newConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(body))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func TestPlanChangesNothing(t *testing.T) {
	logs := t.TempDir()
	file := oldFile(t, logs, "old.log")

	cfg := newConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
tasks:
  - name: logs
    type: logclean
    roots: [%s]
    older_than: 30d
`, filepath.Join(t.TempDir(), "state"), logs))

	rep, err := Run(context.Background(), cfg, Options{Apply: false, Now: fixedNow})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ExitCode != exitcode.OK {
		t.Errorf("exit code = %d, want %d", rep.ExitCode, exitcode.OK)
	}
	if rep.Tasks[0].Planned != 1 {
		t.Errorf("planned = %d, want 1", rep.Tasks[0].Planned)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("a rehearsal must not delete anything: %v", err)
	}
	if rep.Totals.FilesRemoved != 0 {
		t.Errorf("files removed in plan mode = %d", rep.Totals.FilesRemoved)
	}
}

func TestApplyRemovesOnlyExpiredFiles(t *testing.T) {
	logs := t.TempDir()
	expired := oldFile(t, logs, "expired.log")
	recent := filepath.Join(logs, "recent.log")
	if err := os.WriteFile(recent, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := newConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
tasks:
  - name: logs
    type: logclean
    roots: [%s]
    older_than: 30d
`, filepath.Join(t.TempDir(), "state"), logs))

	rep, err := Run(context.Background(), cfg, Options{Apply: true, Now: fixedNow})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ExitCode != exitcode.OK {
		t.Fatalf("exit code = %d (%s)", rep.ExitCode, rep.Status)
	}
	if _, err := os.Stat(expired); err == nil {
		t.Error("expired file should be gone")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("recent file must survive")
	}
	if rep.Totals.FilesRemoved != 1 {
		t.Errorf("files removed = %d, want 1", rep.Totals.FilesRemoved)
	}
	if rep.Mode != report.ModeApply {
		t.Errorf("mode = %q", rep.Mode)
	}
}

func TestSafetyCeilingRefusesTheWholeRun(t *testing.T) {
	logs := t.TempDir()
	var files []string
	for _, name := range []string{"a.log", "b.log", "c.log"} {
		files = append(files, oldFile(t, logs, name))
	}

	cfg := newConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
  safety:
    max_delete_files: 2
tasks:
  - name: logs
    type: logclean
    roots: [%s]
    older_than: 30d
`, filepath.Join(t.TempDir(), "state"), logs))

	rep, err := Run(context.Background(), cfg, Options{Apply: true, Now: fixedNow})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ExitCode != exitcode.Refused {
		t.Errorf("exit code = %d, want %d", rep.ExitCode, exitcode.Refused)
	}
	if rep.Status != report.StatusRefused {
		t.Errorf("status = %q, want refused", rep.Status)
	}
	for _, file := range files {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("nothing may be deleted when the run is refused: %s", file)
		}
	}
}

func TestForceBypassesTheCeiling(t *testing.T) {
	logs := t.TempDir()
	for _, name := range []string{"a.log", "b.log", "c.log"} {
		oldFile(t, logs, name)
	}

	cfg := newConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
  safety:
    max_delete_files: 1
tasks:
  - name: logs
    type: logclean
    roots: [%s]
    older_than: 30d
`, filepath.Join(t.TempDir(), "state"), logs))

	rep, err := Run(context.Background(), cfg, Options{Apply: true, Force: true, Now: fixedNow})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ExitCode != exitcode.OK {
		t.Errorf("exit code = %d (%s)", rep.ExitCode, rep.Status)
	}
	if rep.Totals.FilesRemoved != 3 {
		t.Errorf("files removed = %d, want 3", rep.Totals.FilesRemoved)
	}
}

// backupRunConfig assembles one backup task: a small source tree and a
// destination beside it that does not exist yet.
func backupRunConfig(t *testing.T, src, dest, extra string) *config.Config {
	t.Helper()
	return newConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
tasks:
  - name: data
    type: backup
    roots: [%s]
    destination: %s
%s`, filepath.Join(t.TempDir(), "state"), src, dest, extra))
}

// backupSource writes the tree the backup scenarios below snapshot.
func backupSource(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "app.conf"), []byte("listen=8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "logs", "app.log"), []byte("started\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return src
}

// snapshotsIn lists the snapshot directories of a destination.
func snapshotsIn(t *testing.T, dest string) []string {
	t.Helper()
	entries, err := os.ReadDir(dest)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		t.Fatalf("read %s: %v", dest, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// A backup whose plan exceeds its own ceiling refuses the run with the refused
// exit code, not the ordinary failure code, and writes nothing. The rehearsal
// reports the same refusal, so the ceiling is visible without a run.
func TestBackupCeilingRefusesTheRun(t *testing.T) {
	src := backupSource(t)
	dest := filepath.Join(t.TempDir(), "snapshots")
	cfg := backupRunConfig(t, src, dest, "    max_backup_bytes: \"1B\"\n")

	for _, scenario := range []struct {
		name  string
		apply bool
	}{
		{name: "plan", apply: false},
		{name: "apply", apply: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			rep, err := Run(context.Background(), cfg, Options{Apply: scenario.apply, Now: fixedNow})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rep.ExitCode != exitcode.Refused {
				t.Errorf("exit code = %d, want %d", rep.ExitCode, exitcode.Refused)
			}
			if rep.Status != report.StatusRefused {
				t.Errorf("status = %q, want refused", rep.Status)
			}
			if got := strings.Join(rep.Notes, "\n"); !strings.Contains(got, "max_backup_bytes") {
				t.Errorf("the refusal should name the ceiling, got:\n%s", got)
			}
			// A refusal is not a failure: an operator has a decision to make, not a
			// fault to retry, so the refused task is not counted as an error.
			if rep.Tasks[0].Totals.Errors != 0 {
				t.Errorf("a refusal must not be counted as an error, got %d", rep.Tasks[0].Totals.Errors)
			}
			if _, statErr := os.Stat(dest); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("a refused run must create no destination, stat err = %v", statErr)
			}
		})
	}
}

// --force reaches a task's own ceiling the way it reaches run.safety, and the
// report records the bypass on the task it belonged to.
func TestForceBypassesTheBackupCeiling(t *testing.T) {
	src := backupSource(t)
	dest := filepath.Join(t.TempDir(), "snapshots")
	cfg := backupRunConfig(t, src, dest, "    max_backup_bytes: \"1B\"\n")

	rep, err := Run(context.Background(), cfg, Options{Apply: true, Force: true, Now: fixedNow})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ExitCode != exitcode.OK {
		t.Fatalf("exit code = %d (%s), want 0", rep.ExitCode, rep.Status)
	}
	if got := snapshotsIn(t, dest); len(got) != 1 {
		t.Fatalf("destination holds %v, want the snapshot the forced run wrote", got)
	}
	if rep.Totals.FilesWritten != 2 || rep.Totals.BytesWritten != 20 {
		t.Errorf("totals = %+v, want both files written for 20 B", rep.Totals)
	}
	notes := strings.Join(rep.Tasks[0].Notes, "\n")
	for _, want := range []string{"max_backup_bytes", "bypass"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the task report should record the bypass (%q), got:\n%s", want, notes)
		}
	}
}

// A task whose plan fails for any other reason stays an ordinary failure: only a
// ceiling breach is a refusal, so the two exit codes keep telling an operator
// different things.
func TestPlanFailureIsNotARefusal(t *testing.T) {
	src := backupSource(t)
	dest := filepath.Join(t.TempDir(), "snapshots")
	// A snapshot-shaped directory whose index this build cannot read makes the
	// plan fail. That is a fault to investigate, not a decision to make.
	snapshot := filepath.Join(dest, "20260101T000000Z")
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "manifest.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := backupRunConfig(t, src, dest, "")
	rep, err := Run(context.Background(), cfg, Options{Apply: true, Now: fixedNow})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ExitCode != exitcode.Partial {
		t.Errorf("exit code = %d, want %d", rep.ExitCode, exitcode.Partial)
	}
	if rep.Status == report.StatusRefused {
		t.Errorf("status = %q, want a failure rather than a refusal", rep.Status)
	}
	if got := rep.Tasks[0].Status; got != report.StatusFailed {
		t.Errorf("task status = %q, want failed", got)
	}
}

func TestLockedRunDoesNothing(t *testing.T) {
	logs := t.TempDir()
	file := oldFile(t, logs, "old.log")

	cfg := newConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
tasks:
  - name: logs
    type: logclean
    roots: [%s]
    older_than: 30d
`, filepath.Join(t.TempDir(), "state"), logs))

	handle, err := lock.Acquire(cfg.Run.LockFile)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = handle.Release() }()

	rep, err := Run(context.Background(), cfg, Options{Apply: true, Now: fixedNow})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ExitCode != exitcode.Locked {
		t.Errorf("exit code = %d, want %d", rep.ExitCode, exitcode.Locked)
	}
	if rep.Status != report.StatusSkipped {
		t.Errorf("status = %q, want skipped", rep.Status)
	}
	if _, err := os.Stat(file); err != nil {
		t.Error("a locked run must not delete anything")
	}
}

func TestTaskFilterSelectsOnlyNamedTasks(t *testing.T) {
	logs := t.TempDir()
	kept := oldFile(t, logs, "kept.log")
	removed := oldFile(t, t.TempDir(), "removed.log")

	other := filepath.Dir(removed)
	cfg := newConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
tasks:
  - name: logs
    type: logclean
    roots: [%s]
    older_than: 30d
  - name: other
    type: tempclean
    roots: [%s]
    older_than: 30d
`, filepath.Join(t.TempDir(), "state"), logs, other))

	if _, err := Run(context.Background(), cfg, Options{Apply: true, Only: []string{"other"}, Now: fixedNow}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Error("task logs was not selected and must not run")
	}
	if _, err := os.Stat(removed); err == nil {
		t.Error("task other was selected and should have removed its candidate")
	}
}

func TestUnknownTaskNameIsAnError(t *testing.T) {
	cfg := newConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
tasks:
  - name: logs
    type: logclean
    roots: [%s]
`, filepath.Join(t.TempDir(), "state"), t.TempDir()))

	if _, err := Run(context.Background(), cfg, Options{Only: []string{"nope"}, Now: fixedNow}); err == nil {
		t.Fatal("expected an error for an unknown task name")
	}
}
