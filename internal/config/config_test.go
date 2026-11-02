package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnknownKeyIsAnError is the reason this package exists: a typo must never
// fall back to a default and delete the wrong files.
func TestUnknownKeyIsAnError(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
tasks:
  - name: logs
    type: logclean
    roots: [/var/log]
    older_then: 30d
`))
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	if !strings.Contains(err.Error(), "older_then") {
		t.Fatalf("error should name the offending key, got: %v", err)
	}
}

func TestValidationCollectsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
tasks:
  - name: logs
    type: logclean
    roots: [relative/path]
  - name: logs
    type: ""
    roots: []
`))
	if err == nil {
		t.Fatal("expected validation problems")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	joined := strings.Join(ve.Problems, "\n")
	for _, want := range []string{
		"must be absolute",
		`duplicate task name "logs"`,
		"type is required",
		"roots is required",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems should include %q, got:\n%s", want, joined)
		}
	}
}

func TestMissingVersionIsAnError(t *testing.T) {
	_, err := Parse([]byte(`
tasks:
  - name: logs
    type: logclean
    roots: [/tmp]
`))
	if err == nil || !strings.Contains(err.Error(), "version is required") {
		t.Fatalf("expected a version problem, got: %v", err)
	}
}

func TestDefaults(t *testing.T) {
	root := t.TempDir()
	cfg, err := Parse([]byte(`
version: 1
tasks:
  - name: logs
    type: logclean
    roots: [` + root + `]
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Run.StateDir == "" || !filepath.IsAbs(cfg.Run.StateDir) {
		t.Errorf("state_dir should default to an absolute path, got %q", cfg.Run.StateDir)
	}
	if want := filepath.Join(cfg.Run.StateDir, "cronkite.lock"); cfg.Run.LockFile != want {
		t.Errorf("lock_file = %q, want %q", cfg.Run.LockFile, want)
	}
	if cfg.Report.Dir != filepath.Join(cfg.Run.StateDir, "reports") {
		t.Errorf("report dir = %q", cfg.Report.Dir)
	}
	if cfg.Run.Safety.MaxDeleteFiles != 500 {
		t.Errorf("max_delete_files = %d, want 500", cfg.Run.Safety.MaxDeleteFiles)
	}
	if cfg.Report.MaxActionsPerTask != 1000 {
		t.Errorf("max_actions_per_task = %d, want 1000", cfg.Report.MaxActionsPerTask)
	}
	if got, want := cfg.SafetyBytes(), int64(5<<30); got != want {
		t.Errorf("safety bytes = %d, want %d", got, want)
	}
	if len(cfg.Notify.On) != 2 {
		t.Errorf("notify.on default = %v", cfg.Notify.On)
	}
	if cfg.Tasks[0].IsEnabled() != true {
		t.Error("tasks should be enabled by default")
	}
}

func TestPathExpansion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRONKITE_TEST_ROOT", dir)

	cfg, err := Parse([]byte(`
version: 1
run:
  state_dir: $CRONKITE_TEST_ROOT/state
tasks:
  - name: logs
    type: logclean
    roots:
      - $CRONKITE_TEST_ROOT/logs
      - "%CRONKITE_TEST_ROOT%/logs"
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := filepath.Join(dir, "state"); cfg.Run.StateDir != want {
		t.Errorf("state_dir = %q, want %q", cfg.Run.StateDir, want)
	}
	for _, got := range cfg.Tasks[0].Roots {
		if want := filepath.Join(dir, "logs"); got != want {
			t.Errorf("root = %q, want %q", got, want)
		}
	}
}

// TestUnsetVariableInRootIsInapplicable covers one configuration shared between
// Linux and Windows: "%TEMP%" cannot be an error on a machine that has no
// TEMP variable, and it must not be treated as a root either.
func TestUnsetVariableInRootIsInapplicable(t *testing.T) {
	t.Setenv("CRONKITE_SET_ROOT", t.TempDir())
	cfg, err := Parse([]byte(`
version: 1
tasks:
  - name: temp
    type: tempclean
    roots:
      - $CRONKITE_SET_ROOT
      - "%CRONKITE_TEMP_VAR_THAT_IS_NOT_SET%"
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Tasks[0].Roots) != 1 {
		t.Errorf("roots = %v, want only the resolved one", cfg.Tasks[0].Roots)
	}
	if len(cfg.Tasks[0].UnresolvedRoots) != 1 {
		t.Errorf("unresolved = %v, want the unset variable", cfg.Tasks[0].UnresolvedRoots)
	}
}

func TestAllRootsUnsetIsStillAnError(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
tasks:
  - name: temp
    type: tempclean
    roots: []
`))
	if err == nil || !strings.Contains(err.Error(), "roots is required") {
		t.Fatalf("expected a roots problem, got: %v", err)
	}
}

func TestUnknownNotifyEventIsRejected(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
notify:
  on: [failure, whenever]
tasks:
  - name: logs
    type: logclean
    roots: [/tmp]
`))
	if err == nil || !strings.Contains(err.Error(), "whenever") {
		t.Fatalf("expected an unknown event problem, got: %v", err)
	}
}

func TestLoadReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cronkite.conf")
	if err := os.WriteFile(path, []byte("version: 1\ntasks:\n  - name: logs\n    type: logclean\n    roots: ["+dir+"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(cfg.Tasks))
	}
}

func TestLoadReportsThePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.conf")
	if err := os.WriteFile(path, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error should name the file, got: %v", err)
	}
}
