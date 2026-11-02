package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
