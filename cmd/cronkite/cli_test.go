package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
