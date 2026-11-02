package backup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhuancz/cronkite/internal/config"
	"github.com/jhuancz/cronkite/internal/report"
	"github.com/jhuancz/cronkite/internal/task"
)

func day(n int) time.Time { return time.Date(2026, 3, n, 10, 0, 0, 0, time.UTC) }

func writeFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// backupConfig is one valid backup task: a small source tree and a destination
// beside it that does not exist yet.
func backupConfig(t *testing.T) (config.Task, string, string) {
	t.Helper()
	src := t.TempDir()
	writeFile(t, src, "app.conf", "listen=8080\n")
	writeFile(t, src, filepath.Join("logs", "app.log"), "started\n")
	dest := filepath.Join(t.TempDir(), "snapshots")
	return config.Task{Name: "data", Type: config.TypeBackup, Roots: []string{src}, Destination: dest}, src, dest
}

func newTask(t *testing.T, cfg config.Task) *Task {
	t.Helper()
	tk, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = tk.Close() })
	return tk
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		t.Fatalf("read %s: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func kinds(actions []task.Action) []report.ActionKind {
	out := make([]report.ActionKind, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.Kind)
	}
	return out
}

func notes(plan *task.Plan) string { return strings.Join(plan.Notes, "\n") }

// Plan must report the shape of the work and touch nothing: no destination
// directory, no staging tree, no snapshot.
func TestPlanReportsWorkAndWritesNothing(t *testing.T) {
	cfg, src, dest := backupConfig(t)
	tk := newTask(t, cfg)

	plan, err := tk.Plan(context.Background(), task.Env{Mode: report.ModePlan, Now: day(1)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("a backup plan has no per-file actions, got %+v", plan.Actions)
	}
	got := notes(plan)
	for _, want := range []string{src, dest, "2 files", "20 B", "no previous snapshot"} {
		if !strings.Contains(got, want) {
			t.Errorf("notes should mention %q, got:\n%s", want, got)
		}
	}
	if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("plan mode must not create the destination, stat err = %v", err)
	}
}

// The second plan can tell the operator that a run would relink instead of copy.
func TestPlanNotesThePreviousSnapshot(t *testing.T) {
	cfg, _, dest := backupConfig(t)
	tk := newTask(t, cfg)
	if _, err := tk.Apply(context.Background(), &task.Plan{}, task.Env{Mode: report.ModeApply, Now: day(1)}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	plan, err := tk.Plan(context.Background(), task.Env{Mode: report.ModePlan, Now: day(2)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := notes(plan)
	if !strings.Contains(got, dest) || !strings.Contains(got, "hardlinked") {
		t.Fatalf("notes should name the previous snapshot and the relink, got:\n%s", got)
	}
	if len(names(t, dest)) != 1 {
		t.Fatalf("plan mode must not write a snapshot, destination holds %v", names(t, dest))
	}
}

// A run publishes one complete snapshot, verifies it against the source, and
// reports every file it wrote as one done action of its own.
func TestApplyWritesAndVerifiesOneSnapshot(t *testing.T) {
	cfg, _, dest := backupConfig(t)
	tk := newTask(t, cfg)

	res, err := tk.Apply(context.Background(), &task.Plan{}, task.Env{Mode: report.ModeApply, Now: day(1)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("a fresh snapshot must verify, got failures %+v", res.Failed)
	}
	snapshots := names(t, dest)
	if len(snapshots) != 1 {
		t.Fatalf("destination holds %v, want one snapshot", snapshots)
	}

	snapshot := filepath.Join(dest, snapshots[0])
	for rel, want := range map[string]string{
		"app.conf":                       "listen=8080\n",
		filepath.Join("logs", "app.log"): "started\n",
	} {
		got, err := os.ReadFile(filepath.Join(snapshot, rel))
		if err != nil {
			t.Fatalf("read %s from the snapshot: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("snapshot %s = %q, want %q", rel, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(snapshot, "manifest.json")); err != nil {
		t.Fatalf("the snapshot has no index: %v", err)
	}

	// Files are done work, named one by one with their size, and a first run has
	// nothing to relink against.
	written := map[string]int64{}
	for i, a := range res.Done {
		if res.Done[i].Kind != actionWrite {
			t.Fatalf("first run reported %q for %s, want a write", a.Kind, a.Path)
		}
		if !strings.HasPrefix(a.Path, snapshot+string(filepath.Separator)) {
			t.Errorf("done action path %s is not inside the published snapshot", a.Path)
		}
		written[filepath.ToSlash(strings.TrimPrefix(a.Path, snapshot+string(filepath.Separator)))] = a.Size
	}
	if len(written) != 2 || written["app.conf"] != 12 || written["logs/app.log"] != 8 {
		t.Fatalf("done actions = %v, want both source files with their sizes", written)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("unexpected skips: %+v", res.Skipped)
	}
}

// A rerun writes nothing new and reports the files as relinked, which is what
// makes the backup incremental.
func TestApplyRelinksUnchangedFiles(t *testing.T) {
	cfg, _, dest := backupConfig(t)
	tk := newTask(t, cfg)
	if _, err := tk.Apply(context.Background(), &task.Plan{}, task.Env{Mode: report.ModeApply, Now: day(1)}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	res, err := tk.Apply(context.Background(), &task.Plan{}, task.Env{Mode: report.ModeApply, Now: day(2)})
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("unexpected failures: %+v", res.Failed)
	}
	got := kinds(res.Done)
	if len(got) != 2 {
		t.Fatalf("done actions = %v, want the two relinked files", got)
	}
	for _, kind := range got {
		if kind != actionRelink {
			t.Fatalf("kinds = %v, want every file relinked on an unchanged rerun", got)
		}
	}
	if len(names(t, dest)) != 2 {
		t.Fatalf("both snapshots should be kept, destination holds %v", names(t, dest))
	}
}

// Retention runs after a verified snapshot and deletes whole snapshots, oldest
// first, reporting each one as a removal.
func TestApplyPrunesSnapshotsOutsideRetention(t *testing.T) {
	cfg, _, dest := backupConfig(t)
	cfg.Retention = config.Retention{Daily: intPtr(1), Weekly: intPtr(0)}
	tk := newTask(t, cfg)

	if _, err := tk.Apply(context.Background(), &task.Plan{}, task.Env{Mode: report.ModeApply, Now: day(1)}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	second, err := tk.Apply(context.Background(), &task.Plan{}, task.Env{Mode: report.ModeApply, Now: day(2)})
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	// The second run publishes the newest day and, keeping one day, deletes the
	// snapshot of the day before it.
	if removed := removedSnapshots(t, second); len(removed) != 1 || !strings.HasPrefix(removed[0], "20260301T") {
		t.Fatalf("second run pruned %v, want the previous day's snapshot", removed)
	}

	third, err := tk.Apply(context.Background(), &task.Plan{}, task.Env{Mode: report.ModeApply, Now: day(3)})
	if err != nil {
		t.Fatalf("third Apply: %v", err)
	}
	if len(third.Failed) != 0 {
		t.Fatalf("unexpected failures: %+v", third.Failed)
	}
	kept := names(t, dest)
	if len(kept) != 1 || !strings.HasPrefix(kept[0], "20260303T") {
		t.Fatalf("destination holds %v, want only the newest day's snapshot", kept)
	}
	if removed := removedSnapshots(t, third); len(removed) != 1 || !strings.HasPrefix(removed[0], "20260302T") {
		t.Fatalf("third run pruned %v, want the day before it", removed)
	}
}

// removedSnapshots lists the snapshots a run deleted, in the order it deleted
// them, and checks that each one says why.
func removedSnapshots(t *testing.T, res *task.Result) []string {
	t.Helper()
	var removed []string
	for _, a := range res.Done {
		if a.Kind != report.ActionDelete {
			continue
		}
		if a.Reason == "" {
			t.Errorf("pruned snapshot %s carries no reason", a.Path)
		}
		removed = append(removed, filepath.Base(a.Path))
	}
	return removed
}

// Verification runs before retention, and a snapshot that does not match its
// manifest must not cost the restore points that came before it.
func TestApplyKeepsOldSnapshotsWhenVerificationFails(t *testing.T) {
	cfg, src, dest := backupConfig(t)
	cfg.Retention = config.Retention{Daily: intPtr(1), Weekly: intPtr(0)}
	cfg.VerifySamplePercent = intPtr(100)
	tk := newTask(t, cfg)

	if _, err := tk.Apply(context.Background(), &task.Plan{}, task.Env{Mode: report.ModeApply, Now: day(1)}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	snapshots := names(t, dest)
	if len(snapshots) != 1 {
		t.Fatalf("destination holds %v, want one snapshot", snapshots)
	}

	// Corrupt the published bytes while keeping size and mtime, which is the
	// defect a manifest written from the record alone cannot see: the next run
	// relinks the damaged inode and inherits a digest it never observed.
	damaged := filepath.Join(dest, snapshots[0], "logs", "app.log")
	info, err := os.Stat(damaged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(damaged, []byte("BROKEN!\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(damaged, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	res, err := tk.Apply(context.Background(), &task.Plan{}, task.Env{Mode: report.ModeApply, Now: day(2)})
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(res.Failed) == 0 {
		t.Fatal("a published entry that does not match its manifest must fail the run")
	}
	if !strings.Contains(res.Failed[0].Path, "app.log") {
		t.Fatalf("failure names %s, want the offending path", res.Failed[0].Path)
	}
	if len(names(t, dest)) != 2 {
		t.Fatalf("a failed verification must not prune, destination holds %v", names(t, dest))
	}
	if _, err := os.Stat(filepath.Join(src, "logs", "app.log")); err != nil {
		t.Fatalf("the source tree is untouched: %v", err)
	}
}

// A destination inside the source is refused when the task is built: a snapshot
// that walked the previous snapshot would grow without bound.
func TestNewRefusesDestinationInsideSource(t *testing.T) {
	cfg, src, _ := backupConfig(t)
	cfg.Destination = filepath.Join(src, "snapshots")

	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected a destination inside the source to be refused")
	}
	if !strings.Contains(err.Error(), "inside") {
		t.Fatalf("error should say the destination is inside the source, got: %v", err)
	}
}

// A source that does not exist is refused with a clear message instead of
// writing an empty snapshot.
func TestNewRefusesMissingSource(t *testing.T) {
	cfg, _, _ := backupConfig(t)
	missing := filepath.Join(t.TempDir(), "gone")
	cfg.Roots = []string{missing}

	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected a missing source root to be refused")
	}
	if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error should name the missing root, got: %v", err)
	}
}

// Source entries the writer cannot copy are reported as skipped with the reason,
// never as done work.
func TestApplyReportsSkippedSourceEntries(t *testing.T) {
	cfg, src, _ := backupConfig(t)
	outside := writeFile(t, t.TempDir(), "outside.txt", "not a source file")
	if err := os.Symlink(outside, filepath.Join(src, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	tk := newTask(t, cfg)

	res, err := tk.Apply(context.Background(), &task.Plan{}, task.Env{Mode: report.ModeApply, Now: day(1)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped = %+v, want the symlink", res.Skipped)
	}
	if got := res.Skipped[0].Reason; got != "symlink" {
		t.Fatalf("skip reason = %q, want %q", got, "symlink")
	}
	if !strings.HasSuffix(res.Skipped[0].Path, "link.txt") {
		t.Fatalf("skipped path %s is not the source symlink", res.Skipped[0].Path)
	}
}

func intPtr(v int) *int { return &v }
