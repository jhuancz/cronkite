package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jhuancz/cronkite/internal/report"
)

func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func timeAgo() time.Time { return time.Now().Add(-90 * 24 * time.Hour) }

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cronkite.conf")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVersion(t *testing.T) {
	code, stdout, _ := runCLI("version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "cronkite") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestUnknownCommandIsAnError(t *testing.T) {
	code, _, stderr := runCLI("frobnicate")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	code, stdout, _ := runCLI("help")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, want := range []string{"plan", "run", "check", "report", "exit codes"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help should mention %q", want)
		}
	}
}

func TestMissingConfigurationIsReported(t *testing.T) {
	t.Chdir(t.TempDir())
	code, _, stderr := runCLI("plan")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no configuration file found") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestPlanSucceeds(t *testing.T) {
	logs := t.TempDir()
	cfgPath := writeConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
tasks:
  - name: logs
    type: logclean
    roots: [%s]
`, filepath.Join(t.TempDir(), "state"), logs))

	code, stdout, stderr := runCLI("plan", "-config", cfgPath)
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "logs") || !strings.Contains(stdout, "plan") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestRunWithoutApplyOnlyRehearses(t *testing.T) {
	logs := t.TempDir()
	old := filepath.Join(logs, "old.log")
	if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
tasks:
  - name: logs
    type: logclean
    roots: [%s]
`, filepath.Join(t.TempDir(), "state"), logs))

	code, _, stderr := runCLI("run", "-config", cfgPath)
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	if _, err := os.Stat(old); err != nil {
		t.Error("run without --apply must not delete anything")
	}
}

func TestSafetyRefusalExitCode(t *testing.T) {
	logs := t.TempDir()
	for _, name := range []string{"a.log", "b.log"} {
		path := filepath.Join(logs, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := timeAgo()
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := writeConfig(t, fmt.Sprintf(`
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

	code, stdout, _ := runCLI("run", "--apply", "-config", cfgPath)
	if code != 4 {
		t.Fatalf("exit code = %d, want 4", code)
	}
	if !strings.Contains(stdout, "refused") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestPlanRejectsApply(t *testing.T) {
	code, _, stderr := runCLI("plan", "--apply")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "does not accept --apply") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestInvalidConfigurationIsReported(t *testing.T) {
	cfgPath := writeConfig(t, "version: 1\ntasks:\n  - name: x\n    type: logclean\n    roots: [relative]\n")
	code, _, stderr := runCLI("check", "-config", cfgPath)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "must be absolute") {
		t.Errorf("stderr = %q", stderr)
	}
}

// TestCheckToleratesCrossPlatformRoots covers the shared configuration case: a
// root for the other platform must warn, not fail.
func TestCheckToleratesCrossPlatformRoots(t *testing.T) {
	existing := t.TempDir()
	cfgPath := writeConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
tasks:
  - name: temp
    type: tempclean
    roots:
      - %s
      - "%%CRONKITE_NOT_SET_ANYWHERE%%"
`, filepath.Join(t.TempDir(), "state"), existing))

	code, stdout, stderr := runCLI("check", "-config", cfgPath)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "WARN") {
		t.Errorf("stdout should warn about the unset root, got:\n%s", stdout)
	}
}

func TestCheckFailsWhenNoRootExists(t *testing.T) {
	cfgPath := writeConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
tasks:
  - name: logs
    type: logclean
    roots: [%s]
`, filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "gone")))

	code, _, _ := runCLI("check", "-config", cfgPath)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestJSONReportIsMachineReadable(t *testing.T) {
	logs := t.TempDir()
	cfgPath := writeConfig(t, fmt.Sprintf(`
version: 1
run:
  state_dir: %s
tasks:
  - name: logs
    type: logclean
    roots: [%s]
`, filepath.Join(t.TempDir(), "state"), logs))

	code, stdout, stderr := runCLI("plan", "-config", cfgPath, "-json")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{`"schema": 1`, `"mode": "plan"`, `"status": "ok"`, `"run_id"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("json report should contain %s, got:\n%s", want, stdout)
		}
	}
}

// writeBackupConfig writes a configuration with one backup task that snapshots
// src into dest. extra is appended to the task block, already indented.
func writeBackupConfig(t *testing.T, src, dest, extra string) string {
	t.Helper()
	return writeConfig(t, fmt.Sprintf(`
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

// backupSource writes the tree the backup scenarios below snapshot: two files in
// two directories and one empty directory, so a restore has a tree to reproduce
// and not only bytes.
func backupSource(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	for rel, body := range map[string]string{
		"app.conf":                       "listen=8080\n",
		filepath.Join("logs", "app.log"): "started\n",
	} {
		path := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(src, "queue"), 0o755); err != nil {
		t.Fatal(err)
	}
	return src
}

// snapshotNames lists the snapshots a destination holds, oldest first.
func snapshotNames(t *testing.T, dest string) []string {
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
	sort.Strings(out)
	return out
}

// backdateSnapshot renames the snapshot a run just wrote to a name the writer
// would have produced at a plainly earlier time.
//
// Snapshot names have one-second resolution and a test cannot wait for the clock,
// so two runs of the same tree would collide on the same name; renaming is also
// how a scenario asks for two snapshots far enough apart for retention to tell
// them apart. The name is the layout internal/backup writes.
func backdateSnapshot(t *testing.T, dest, name, older string) {
	t.Helper()
	if err := os.Rename(filepath.Join(dest, name), filepath.Join(dest, older)); err != nil {
		t.Fatalf("backdate snapshot %s: %v", name, err)
	}
}

// copyTree copies a directory the way "cp -a" does, directories and contents
// alike, so a published snapshot can be restored and compared with the source.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy %s to %s: %v", src, dst, err)
	}
}

// treeEntries describes a tree as one map: every path relative to root, mapped to
// "<dir>" or to the file's bytes. Two trees are equal when their maps are.
func treeEntries(t *testing.T, root string, ignore map[string]bool) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if ignore[rel] {
			return nil
		}
		if d.IsDir() {
			out[rel] = "<dir>"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// decodeReport reads the JSON report a command printed, so a scenario can assert
// the counters of a run instead of its prose.
func decodeReport(t *testing.T, stdout string) report.Report {
	t.Helper()
	var rep report.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout)
	}
	return rep
}

// hardlinksAvailable reports whether this filesystem can publish a relink at all.
// A platform that cannot falls back to copying by design, so a scenario about
// saving a copy is skipped there instead of failing.
func hardlinksAvailable(t *testing.T) bool {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return os.Link(probe, probe+".link") == nil
}

// Scenario: a run publishes one snapshot that a plain copy restores into a tree
// equal to the source.
//
// This and the scenarios below drive run() directly rather than a txtar script
// because testscript cannot reach them: its cp copies files (it has no
// recursive "cp -a"), it has no globbing, and a snapshot directory is named by
// the second a run happens in, so a script has no way to name the snapshot it
// just wrote.
func TestBackupRunPublishesARestorableSnapshot(t *testing.T) {
	src := backupSource(t)
	dest := filepath.Join(t.TempDir(), "snapshots")
	cfgPath := writeBackupConfig(t, src, dest, "")

	code, stdout, stderr := runCLI("run", "--apply", "-config", cfgPath)
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "2 files written") {
		t.Errorf("the run should report the work as written files, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "removed") {
		// A backup must never be summarised as a removal: it deletes nothing.
		t.Errorf("the run must not describe a backup as a removal, got:\n%s", stdout)
	}

	snapshots := snapshotNames(t, dest)
	if len(snapshots) != 1 {
		t.Fatalf("destination holds %v, want one snapshot", snapshots)
	}
	snapshot := filepath.Join(dest, snapshots[0])

	// The snapshot holds the tree plus its own index; the index is cronkite's
	// bookkeeping and is not part of the tree it restores.
	restored := filepath.Join(t.TempDir(), "restored")
	copyTree(t, snapshot, restored)

	want := treeEntries(t, src, nil)
	got := treeEntries(t, restored, map[string]bool{"manifest.json": true})
	if len(want) != len(got) {
		t.Fatalf("restored tree holds %d entries, the source %d:\n%v\n%v", len(got), len(want), got, want)
	}
	for rel, wantValue := range want {
		if gotValue, ok := got[rel]; !ok || gotValue != wantValue {
			t.Errorf("restored %s = %q (present: %v), want %q", rel, gotValue, ok, wantValue)
		}
	}
}

// Scenario: a second run with no changes copies no bytes. The report counts the
// files it relinked instead of bytes it wrote.
func TestBackupRerunCopiesNoBytes(t *testing.T) {
	if !hardlinksAvailable(t) {
		t.Skip("this filesystem cannot hardlink, so a rerun copies by design")
	}
	src := backupSource(t)
	dest := filepath.Join(t.TempDir(), "snapshots")
	cfgPath := writeBackupConfig(t, src, dest, "")

	if code, _, stderr := runCLI("run", "--apply", "-config", cfgPath); code != 0 {
		t.Fatalf("first run: exit %d (stderr: %s)", code, stderr)
	}
	backdateSnapshot(t, dest, snapshotNames(t, dest)[0], "20200101T000000Z")

	code, stdout, stderr := runCLI("run", "--apply", "-config", cfgPath, "-json")
	if code != 0 {
		t.Fatalf("second run: exit %d (stderr: %s)", code, stderr)
	}
	rep := decodeReport(t, stdout)
	if rep.Status != report.StatusOK || rep.ExitCode != 0 {
		t.Fatalf("second run: status %q, exit %d, want ok", rep.Status, rep.ExitCode)
	}
	totals := rep.Tasks[0].Totals
	if totals.FilesRelinked != 2 {
		t.Errorf("files_relinked = %d, want both unchanged files", totals.FilesRelinked)
	}
	if totals.FilesWritten != 0 || totals.BytesWritten != 0 {
		t.Errorf("bytes_written = %d over %d files, want 0: an unchanged rerun copies nothing",
			totals.BytesWritten, totals.FilesWritten)
	}
	if totals.BytesRelinked == 0 {
		t.Error("the rerun should report the bytes it reused")
	}
}

// Scenario: retention prunes an older snapshot and keeps the one just written.
func TestBackupRetentionPrunesTheOlderSnapshot(t *testing.T) {
	src := backupSource(t)
	dest := filepath.Join(t.TempDir(), "snapshots")
	cfgPath := writeBackupConfig(t, src, dest, "    retention:\n      daily: 1\n      weekly: 0\n")

	if code, _, stderr := runCLI("run", "--apply", "-config", cfgPath); code != 0 {
		t.Fatalf("first run: exit %d (stderr: %s)", code, stderr)
	}
	const older = "20200101T000000Z"
	backdateSnapshot(t, dest, snapshotNames(t, dest)[0], older)

	code, stdout, stderr := runCLI("run", "--apply", "-config", cfgPath, "-json")
	if code != 0 {
		t.Fatalf("second run: exit %d (stderr: %s)", code, stderr)
	}

	if _, err := os.Stat(filepath.Join(dest, older)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the snapshot outside retention is still there, stat err = %v", err)
	}
	kept := snapshotNames(t, dest)
	if len(kept) != 1 || kept[0] == older {
		t.Fatalf("destination holds %v, want only the snapshot just written", kept)
	}

	// The report says which snapshot went and which rule condemned it, so the
	// deletion is auditable and not inferred from a directory listing.
	rep := decodeReport(t, stdout)
	var pruned bool
	for _, a := range rep.Tasks[0].Actions {
		if a.Action == report.ActionDelete && filepath.Base(a.Path) == older && a.Reason == "outside_retention" {
			pruned = true
		}
	}
	if !pruned {
		t.Errorf("report actions = %+v, want the pruned snapshot with its reason", rep.Tasks[0].Actions)
	}
}

// Scenario: a ceiling breach exits 4 and creates no destination, and the same
// command with --force succeeds and records the bypass.
//
// testscript can only assert that a command failed, not with which code, so the
// exact code is checked here.
func TestBackupCeilingExitCode(t *testing.T) {
	src := backupSource(t)
	dest := filepath.Join(t.TempDir(), "snapshots")
	cfgPath := writeBackupConfig(t, src, dest, "    max_backup_bytes: \"1B\"\n")

	code, stdout, _ := runCLI("run", "--apply", "-config", cfgPath)
	if code != 4 {
		t.Fatalf("exit code = %d, want 4 (refused)", code)
	}
	for _, want := range []string{"status=refused", "max_backup_bytes", "1 B", "Raise max_backup_bytes"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the refusal should mention %q, got:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused run must create no destination, stat err = %v", err)
	}

	code, stdout, stderr := runCLI("run", "--apply", "-config", cfgPath, "-force", "-json")
	if code != 0 {
		t.Fatalf("forced run: exit %d (stderr: %s)", code, stderr)
	}
	rep := decodeReport(t, stdout)
	if notes := strings.Join(rep.Tasks[0].Notes, "\n"); !strings.Contains(notes, "bypass") || !strings.Contains(notes, "max_backup_bytes") {
		t.Errorf("the report should record the bypassed ceiling, got:\n%s", notes)
	}
	if rep.Totals.FilesWritten != 2 {
		t.Errorf("files_written = %d, want both files of the forced snapshot", rep.Totals.FilesWritten)
	}
	if len(snapshotNames(t, dest)) != 1 {
		t.Errorf("destination holds %v, want the snapshot the forced run wrote", snapshotNames(t, dest))
	}
}

// Scenario: tampering with a file inside a published snapshot makes the next run
// fail verification, and the report names the offending path.
func TestBackupTamperedSnapshotFailsVerification(t *testing.T) {
	src := backupSource(t)
	dest := filepath.Join(t.TempDir(), "snapshots")
	cfgPath := writeBackupConfig(t, src, dest, "    verify_sample_percent: 100\n")

	if code, _, stderr := runCLI("run", "--apply", "-config", cfgPath); code != 0 {
		t.Fatalf("first run: exit %d (stderr: %s)", code, stderr)
	}
	backdateSnapshot(t, dest, snapshotNames(t, dest)[0], "20200101T000000Z")
	snapshot := filepath.Join(dest, "20200101T000000Z")

	// One byte of a published file changes while its size and mtime stay as the
	// manifest records them. The next run relinks that inode and inherits a digest
	// it never observed, so only re-hashing the published bytes can notice.
	tampered := filepath.Join(snapshot, "logs", "app.log")
	info, err := os.Stat(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tampered, []byte("STARTED\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tampered, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("run", "--apply", "-config", cfgPath, "-json")
	if code != 2 {
		t.Fatalf("exit code = %d (stderr: %s), want 2 (partial)", code, stderr)
	}
	rep := decodeReport(t, stdout)
	if rep.Status != report.StatusPartial {
		t.Errorf("status = %q, want partial", rep.Status)
	}
	failures := rep.Tasks[0].Errors
	if len(failures) == 0 {
		t.Fatalf("the run reported no failure for the tampered snapshot")
	}
	got := failures[0]
	// The failing entry is the one this run just published: it relinked the tampered
	// inode, so the new snapshot holds bytes its manifest never recorded.
	if !strings.Contains(filepath.ToSlash(got.Path), "/logs/app.log") ||
		!strings.HasPrefix(filepath.ToSlash(got.Path), filepath.ToSlash(dest)+"/") {
		t.Errorf("failure names %q, want the tampered path in the snapshot published inside %s", got.Path, dest)
	}
	if !strings.Contains(got.Error, "does not match its manifest") {
		t.Errorf("failure error = %q, want it to say what did not match", got.Error)
	}
}
