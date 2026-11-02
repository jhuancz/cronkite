//go:build unix

package script

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jhuancz/cronkite/internal/config"
	"github.com/jhuancz/cronkite/internal/report"
	"github.com/jhuancz/cronkite/internal/task"
)

func envFor(mode report.Mode) task.Env {
	return task.Env{Mode: mode, Now: clock}
}

// scriptConfig writes a bash script into a fresh temporary directory and returns
// a task configuration that runs it.
func scriptConfig(t *testing.T, name, body string) config.Task {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".sh")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return config.Task{Name: name, Type: config.TypeScript, Interpreter: "bash", File: path}
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

func plan(t *testing.T, tk *Task, mode report.Mode) *task.Plan {
	t.Helper()
	p, err := tk.Plan(context.Background(), envFor(mode))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return p
}

// applyRun runs the task the way the engine does: plan first, then apply. The
// plan is returned because the report is assembled from it, including the notes
// a run appends while it executes.
func applyRun(t *testing.T, tk *Task) (*task.Plan, *task.Result) {
	t.Helper()
	p := plan(t, tk, report.ModeApply)
	res, err := tk.Apply(context.Background(), p, envFor(report.ModeApply))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return p, res
}

func notes(list []string) string { return strings.Join(list, "\n") }

func requireNoFile(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s: %s exists (stat error: %v)", why, path, err)
	}
}

// A rehearsal is the script's own preview: plan mode runs it with
// CRONKITE_DRY_RUN=1 and records what it said. A script that honours the variable
// changes nothing, which is what makes the preview trustworthy.
func TestPlanRehearsesWithDryRun(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	out := filepath.Join(t.TempDir(), "env.txt")
	body := fmt.Sprintf(`if [ "$CRONKITE_DRY_RUN" = "1" ]; then
  echo "would touch nothing"
else
  echo touched > %q
fi
printf '%%s\n' "$CRONKITE_DRY_RUN" "$CRONKITE_MODE" > %q
`, marker, out)
	tk := newTask(t, scriptConfig(t, "maint", body))

	p := plan(t, tk, report.ModePlan)

	requireNoFile(t, marker, "a script that honours CRONKITE_DRY_RUN changed something")
	joined := notes(p.Notes)
	for _, want := range []string{"would touch nothing", "DRY_RUN=1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("plan notes should carry %q, got:\n%s", want, joined)
		}
	}
	if len(p.Actions) != 0 {
		t.Errorf("plan actions = %v, want none: a script's changes are its own", p.Actions)
	}
	// The script really ran and reported its own rehearsal mode.
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the rehearsal did not run: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != "1\nplan" {
		t.Errorf("script saw dry_run/mode = %q, want \"1\\nplan\"", got)
	}
}

// The documented cost of a rehearsed plan: a script that ignores
// CRONKITE_DRY_RUN can change things during a rehearsal. Pinning it here keeps the
// contract honest instead of pretending cronkite can enforce what it cannot.
func TestPlanRunsAScriptThatIgnoresTheDryRun(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	tk := newTask(t, scriptConfig(t, "maint", "echo ran > "+marker+"\n"))

	p := plan(t, tk, report.ModePlan)

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a script that ignores CRONKITE_DRY_RUN is expected to run during a rehearsal: %v", err)
	}
	if !strings.Contains(notes(p.Notes), "ran /") {
		t.Errorf("the rehearsal output should still be recorded, got:\n%s", notes(p.Notes))
	}
}

// Apply mode must not rehearse: the engine plans and then applies, so executing in
// both phases would run the script twice.
func TestApplyPhaseDoesNotExecute(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	tk := newTask(t, scriptConfig(t, "maint", "echo ran >> "+marker+"\n"))

	plan(t, tk, report.ModeApply)

	requireNoFile(t, marker, "the apply-phase plan executed the script")
}

// A script that exits 0 is an ok task, and its output is in the report instead
// of being printed at the terminal.
func TestApplyReportsSuccessForExitZero(t *testing.T) {
	tk := newTask(t, scriptConfig(t, "maint", "echo hello from the script\necho a warning >&2\n"))

	p, res := applyRun(t, tk)

	if len(res.Failed) != 0 {
		t.Fatalf("failures = %v, want none", res.Failed)
	}
	if len(res.Done) != 0 {
		t.Errorf("done actions = %v, want none: cronkite did not measure any file change", res.Done)
	}
	joined := notes(append(p.Notes, res.Notes...))
	for _, want := range []string{"hello from the script", "a warning"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes should carry %q, got:\n%s", want, joined)
		}
	}
}

// The arguments are handed to the script after the file, and the environment
// says which run this is.
func TestApplyPassesArgsAndTheRunEnvironment(t *testing.T) {
	out := filepath.Join(t.TempDir(), "env.txt")
	cfg := scriptConfig(t, "maint", `printf '%s\n' "$CRONKITE_MODE" "$CRONKITE_DRY_RUN" "$CRONKITE_TASK" "$CRONKITE_RUN_ID" > "$1"
printf 'arg:%s\n' "$2" >> "$1"
`)
	cfg.Args = []string{out, "--rotate now"}
	tk := newTask(t, cfg)

	_, res := applyRun(t, tk)
	if len(res.Failed) != 0 {
		t.Fatalf("failures = %v, want none", res.Failed)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the file the script wrote: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 5 {
		t.Fatalf("the script wrote %d lines, want 5:\n%s", len(lines), data)
	}
	if lines[0] != "apply" {
		t.Errorf("CRONKITE_MODE = %q, want %q", lines[0], "apply")
	}
	if lines[1] != "0" {
		t.Errorf("CRONKITE_DRY_RUN = %q, want %q", lines[1], "0")
	}
	if lines[2] != "maint" {
		t.Errorf("CRONKITE_TASK = %q, want %q", lines[2], "maint")
	}
	if !regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`).MatchString(lines[3]) {
		t.Errorf("CRONKITE_RUN_ID = %q, want a run timestamp", lines[3])
	}
	if lines[4] != "arg:--rotate now" {
		t.Errorf("second argument = %q, want it passed through as one argument", lines[4])
	}
}

// A non-zero exit is a task failure that carries the script's own status and the
// tail of its output: the script's message is usually the explanation.
func TestApplyReportsNonZeroExitWithItsStatus(t *testing.T) {
	tk := newTask(t, scriptConfig(t, "maint", "echo 'rotation failed: no space left' >&2\nexit 3\n"))

	_, res := applyRun(t, tk)

	if len(res.Failed) != 1 {
		t.Fatalf("failures = %v, want one", res.Failed)
	}
	if got, want := res.Failed[0].Path, tk.file; got != want {
		t.Errorf("failure path = %q, want the script %q", got, want)
	}
	msg := res.Failed[0].Err.Error()
	for _, want := range []string{"status 3", "no space left"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure should mention %q, got: %v", want, msg)
		}
	}
}

// A script that never returns is stopped, and the process group is stopped with
// it: the file the script's own child would write after the sleep never appears.
func TestApplyKillsTheProcessGroupOnTimeout(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "late")
	cfg := scriptConfig(t, "maint", "( sleep 0.5; echo late > "+marker+" ) &\nsleep 5\n")
	cfg.Timeout = "150ms"
	tk := newTask(t, cfg)

	started := time.Now()
	_, res := applyRun(t, tk)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Apply took %s, want it stopped by the 150ms timeout", elapsed)
	}

	if len(res.Failed) != 1 {
		t.Fatalf("failures = %v, want one", res.Failed)
	}
	if msg := res.Failed[0].Err.Error(); !strings.Contains(msg, "150ms") || !strings.Contains(msg, "process group") {
		t.Errorf("failure should say the timeout killed the process group, got: %v", msg)
	}

	// The child sleeps 0.5s and would write the marker well after the kill.
	time.Sleep(800 * time.Millisecond)
	requireNoFile(t, marker, "the script's child survived the timeout")
}

// A file that is not there is a failure of this task with a clear message, in
// both phases, and never a panic.
func TestPlanReportsAMissingScriptFile(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone.sh")
	tk := newTask(t, config.Task{Name: "maint", Type: config.TypeScript, Interpreter: "bash", File: gone})

	_, err := tk.Plan(context.Background(), envFor(report.ModePlan))
	if err == nil {
		t.Fatal("expected a missing script file to be reported")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error should wrap the missing file, got: %v", err)
	}
	if !strings.Contains(err.Error(), gone) || !strings.Contains(err.Error(), "maint") {
		t.Errorf("error should name the task and the file, got: %v", err)
	}
}

// A directory is not a script: the path is validated to be a regular file before
// anything runs.
func TestPlanRejectsANonRegularFile(t *testing.T) {
	tk := newTask(t, config.Task{Name: "maint", Type: config.TypeScript, Interpreter: "bash", File: t.TempDir()})

	_, err := tk.Plan(context.Background(), envFor(report.ModePlan))
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("expected a regular-file problem, got: %v", err)
	}
}

// The cap keeps one noisy script from filling the report, and the note says the
// output was cut so nobody reads a truncated report as the whole truth.
func TestOutputIsCappedAndTheTruncationIsNoted(t *testing.T) {
	tk := newTask(t, scriptConfig(t, "maint", `i=0
while [ $i -lt 4000 ]; do
  echo "a line of output from a noisy maintenance script"
  i=$((i+1))
done
`))

	p, res := applyRun(t, tk)

	if len(res.Failed) != 0 {
		t.Fatalf("failures = %v, want none", res.Failed)
	}
	joined := notes(append(p.Notes, res.Notes...))
	if !strings.Contains(joined, "truncated") {
		t.Errorf("notes should say the output was truncated, got %d notes", len(res.Notes))
	}
	if len(joined) > 4*maxOutputBytes {
		t.Errorf("notes hold %d bytes, want at most %d", len(joined), 4*maxOutputBytes)
	}
	if !strings.Contains(joined, "noisy maintenance script") {
		t.Error("the kept output should still be in the report")
	}
}

// The interpreter is resolved from PATH when the task is built, so a missing one
// is a configuration problem instead of a run that fails at three in the
// morning.
func TestNewResolvesTheInterpreter(t *testing.T) {
	file := filepath.Join(t.TempDir(), "maintenance.sh")

	t.Run("interpreter not supported", func(t *testing.T) {
		_, err := New(config.Task{Name: "maint", Type: config.TypeScript, Interpreter: "zsh", File: file})
		if err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("expected an unsupported interpreter error, got: %v", err)
		}
	})

	t.Run("interpreter not on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := New(config.Task{Name: "maint", Type: config.TypeScript, Interpreter: "bash", File: file})
		if err == nil {
			t.Fatal("expected a missing interpreter to be refused at construction")
		}
		for _, want := range []string{"bash", "PATH"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q, got: %v", want, err)
			}
		}
	})
}
