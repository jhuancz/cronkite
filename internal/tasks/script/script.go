// Package script implements the "script" task type: one file the operator names,
// run with one interpreter, under the run lock and the run report.
//
// A site that already keeps a maintenance script should not need a second cron
// entry with its own logging, locking and alerting. Wrapping that script as a
// cronkite task puts its output in the same report as the cleanup and backup
// tasks, which is the point: one run, one audit trail.
//
// The task is deliberately thin. cronkite starts the process, hands it the
// environment that describes the run, and reports what it printed. It does not
// parse the output, guess which files changed, or retry anything. Three
// properties follow from that, and they are what an operator relies on:
//
//   - Plan mode runs nothing. The script is executed only in Apply, so a
//     rehearsal cannot mutate the machine it is rehearsing. Plan reports the
//     command line the run would use and no actions.
//   - A run reports no actions. What a script touches is its own business:
//     inventing per-file actions nobody measured would put fiction in the
//     report. The record of a run is its notes - the command line, the captured
//     output, how long it took - and, when it went wrong, its exit status.
//   - The environment names the run. CRONKITE_RUN_ID, CRONKITE_MODE,
//     CRONKITE_DRY_RUN and CRONKITE_TASK let a script tell a rehearsal from a
//     real run and log the run it belongs to, with the inherited environment
//     passed through untouched.
package script

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jhuancz/cronkite/internal/bytesx"
	"github.com/jhuancz/cronkite/internal/config"
	"github.com/jhuancz/cronkite/internal/durationx"
	"github.com/jhuancz/cronkite/internal/report"
	"github.com/jhuancz/cronkite/internal/task"
)

const (
	// maxOutputBytes caps the output of one stream that reaches the report. A
	// maintenance script can print a line per file for an hour; the report is a
	// record, not a log sink, and the rest belongs in the script's own log file.
	// The cap bounds what is kept, never what is read: a noisy script still runs
	// to completion and a note says how much was dropped.
	maxOutputBytes = 8 << 10
	// maxTailBytes is the most output a failure message carries. A failure is one
	// line in a report and in an alert, so the message carries the end of the
	// output, which is where a script usually says what went wrong.
	maxTailBytes = 512
	// waitDelay bounds how long a run waits for the output pipes to close once
	// the script itself is gone. It only matters when a child the script started
	// inherited the pipes and outlived it; without it, that child could hold a
	// run open past its timeout.
	waitDelay = 2 * time.Second
)

// Task runs one operator-supplied script file with one interpreter.
type Task struct {
	name        string
	file        string
	interpreter string
	// interpreterPath is where the interpreter was found at construction, so a
	// run does not have to discover that it is missing.
	interpreterPath string
	args            []string
	timeout         time.Duration
}

var _ task.Runner = (*Task)(nil)

// New validates the task configuration and resolves the interpreter from PATH.
//
// The interpreter is resolved here, at construction, on purpose: a scheduled run
// that discovers a missing interpreter has already spent its slot, and an
// operator reading the report at three in the morning is the wrong person to
// debug PATH. Whether the script file exists is a run-time question, answered by
// Plan and by Apply before anything is started.
func New(cfg config.Task) (*Task, error) {
	if cfg.Type != config.TypeScript {
		return nil, fmt.Errorf("unknown task type %q", cfg.Type)
	}

	interpreters := strings.Join(config.ScriptInterpreters, ", ")
	switch {
	case cfg.Interpreter == "":
		return nil, fmt.Errorf("task %s: interpreter is required for a script task (want one of: %s)", cfg.Name, interpreters)
	case !config.IsScriptInterpreter(cfg.Interpreter):
		return nil, fmt.Errorf("task %s: interpreter %q is not supported (want one of: %s)", cfg.Name, cfg.Interpreter, interpreters)
	}

	switch {
	case cfg.File == "":
		return nil, fmt.Errorf("task %s: file is required for a script task", cfg.Name)
	case !filepath.IsAbs(cfg.File):
		// A relative path would be read relative to whatever directory cronkite
		// happens to run from, which is not something a cron job can promise.
		return nil, fmt.Errorf("task %s: script file %q must be absolute", cfg.Name, cfg.File)
	}

	path, err := exec.LookPath(cfg.Interpreter)
	if err != nil {
		return nil, fmt.Errorf("task %s: interpreter %q is not on PATH: %w; install it before the run, so a scheduled run does not find out instead", cfg.Name, cfg.Interpreter, err)
	}

	timeoutText := cfg.Timeout
	if timeoutText == "" {
		timeoutText = config.DefaultScriptTimeout
	}
	timeout, err := durationx.Parse(timeoutText)
	if err != nil {
		return nil, fmt.Errorf("task %s: timeout: %w", cfg.Name, err)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("task %s: timeout %q must be positive (a run that expires at once cannot do anything)", cfg.Name, timeoutText)
	}

	return &Task{
		name:            cfg.Name,
		file:            cfg.File,
		interpreter:     cfg.Interpreter,
		interpreterPath: path,
		args:            append([]string(nil), cfg.Args...),
		timeout:         timeout,
	}, nil
}

// Name implements task.Runner.
func (t *Task) Name() string { return t.name }

// Type implements task.Runner.
func (t *Task) Type() string { return config.TypeScript }

// Close implements task.Runner. A script task holds nothing open between phases.
func (t *Task) Close() error { return nil }

// Plan reports what a run would execute and changes nothing.
//
// The script is not run, not even with CRONKITE_DRY_RUN=1: a rehearsal that ran
// it would be trusting every script on the machine to behave. What the plan
// carries is the exact command line Apply will use, the timeout that bounds it,
// and the environment it will see, so an operator can check the rehearsal agrees
// with the run.
//
// There is one deliberate absence: no actions. cronkite cannot know which files a
// script will touch, and a plan that invented them would offer a preview of work
// nobody measured.
func (t *Task) Plan(ctx context.Context, env task.Env) (*task.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := t.checkFile(); err != nil {
		return nil, fmt.Errorf("task %s: %w", t.name, err)
	}

	plan := &task.Plan{}
	plan.Notes = append(plan.Notes,
		"would run: "+t.commandLine(),
		fmt.Sprintf("timeout %s: exceeding it kills the process group, so nothing the script started outlives the run", t.timeout),
		"environment: "+strings.Join(t.environ(env), " "),
		"no actions are planned: the script decides what it touches, and its output is recorded in this task's notes",
	)
	return plan, nil
}

// Apply runs the script once and reports what happened.
//
// A non-zero exit and a timeout are task failures, not errors: the run
// continues with the next task, exactly as a failed deletion does, and the
// report says which task failed and why.
//
// The output and the summary are recorded in result.Notes, which the engine
// merges into the report next to the plan's notes: the script produced no
// actions, so its output is the only evidence of what it did.
//
// Nothing is reported as done work: cronkite measured no file change, so
// result.Done stays empty in both directions and the report shows the script
// through its notes. Claiming an action here would put work in the totals that
// nobody counted.
func (t *Task) Apply(ctx context.Context, plan *task.Plan, env task.Env) (*task.Result, error) {
	res := &task.Result{}
	if err := t.checkFile(); err != nil {
		res.Failed = append(res.Failed, task.Failure{Path: t.file, Err: err})
		return res, nil
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	stdout := &capture{limit: maxOutputBytes}
	stderr := &capture{limit: maxOutputBytes}

	runCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, t.interpreterPath, append([]string{t.file}, t.args...)...)
	cmd.Env = append(os.Environ(), t.environ(env)...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay
	// The default behaviour of CommandContext kills only the process it started.
	// A script that spawned work of its own would survive that kill and outlive
	// the run, so the whole group is killed instead.
	cmd.Cancel = func() error {
		kill(cmd)
		return nil
	}
	prepare(cmd)

	started := time.Now()
	if err := cmd.Start(); err != nil {
		res.Failed = append(res.Failed, task.Failure{Path: t.file, Err: fmt.Errorf("start %s: %w", t.interpreterPath, err)})
		return res, nil
	}
	waitErr := cmd.Wait()
	elapsed := time.Since(started)

	// The output goes into the report whatever happened: it is the only record of
	// what the script did, and it is never printed to the terminal verbatim.
	res.Notes = append(res.Notes, outputNotes(stdout, stderr)...)
	res.Notes = append(res.Notes, fmt.Sprintf("ran %s in %s", t.commandLine(), elapsed.Round(time.Millisecond)))

	if err := ctx.Err(); err != nil {
		// The run itself was interrupted. That is not the script's failure and is
		// reported as an interrupted run, the same way every other task does it.
		return res, fmt.Errorf("task %s: %w", t.name, err)
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.Failed = append(res.Failed, task.Failure{Path: t.file, Err: fmt.Errorf(
			"timed out after %s: the process group was killed%s", t.timeout, tailSuffix(stdout, stderr))})
		return res, nil
	}
	if waitErr != nil {
		res.Failed = append(res.Failed, task.Failure{Path: t.file, Err: exitFailure(waitErr, stdout, stderr)})
	}
	return res, nil
}

// checkFile validates the trust rule of this task type: the script is a file the
// operator named, so it has to be there and it has to be a regular file. A
// directory or a device is a configuration mistake, not something to hand to an
// interpreter.
func (t *Task) checkFile() error {
	info, err := os.Stat(t.file)
	if err != nil {
		return fmt.Errorf("script %s: %w", t.file, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("script %s is not a regular file (%s); cronkite runs a file it can read", t.file, info.Mode())
	}
	return nil
}

// environ is what this run adds to the inherited environment.
//
// os/exec keeps the last value of a duplicated key, so these win over an
// inherited CRONKITE_* of the same name: a script must see the run it is part of,
// not a variable leaked from whatever started cronkite.
func (t *Task) environ(env task.Env) []string {
	dryRun := "0"
	if env.Mode == report.ModePlan {
		dryRun = "1"
	}
	return []string{
		"CRONKITE_RUN_ID=" + runID(env),
		"CRONKITE_MODE=" + string(env.Mode),
		"CRONKITE_DRY_RUN=" + dryRun,
		"CRONKITE_TASK=" + t.name,
	}
}

// runID is the identifier a script sees as CRONKITE_RUN_ID.
//
// task.Env carries the run's reference time but not the report's identifier, so
// the value is the run id of the report this run will write, so a script's own
// log can be matched against it. The timestamp form is only a fallback for a task
// driven directly by a test, where no run exists.
func runID(env task.Env) string {
	if env.RunID != "" {
		return env.RunID
	}
	now := env.Now
	if now.IsZero() {
		now = time.Now()
	}
	return now.UTC().Format("20060102T150405Z")
}

// commandLine is the command a run executes, quoted the way an operator would
// retype it. Plan reports it and Apply repeats it, so a rehearsal and its run
// agree about what was started.
func (t *Task) commandLine() string {
	parts := make([]string, 0, len(t.args)+2)
	parts = append(parts, quoteArg(t.interpreterPath), quoteArg(t.file))
	for _, arg := range t.args {
		parts = append(parts, quoteArg(arg))
	}
	return strings.Join(parts, " ")
}

func quoteArg(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\"'") {
		return strconv.Quote(s)
	}
	return s
}

// capture keeps the first limit bytes written to it and counts all of them.
//
// Write never fails and never blocks: the script must run to completion whatever
// happens to its output, and a broken pipe is not a useful thing to report about
// a maintenance script.
type capture struct {
	limit int
	buf   bytes.Buffer
	total int
}

func (c *capture) Write(p []byte) (int, error) {
	c.total += len(p)
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (c *capture) String() string { return c.buf.String() }

// Truncated reports whether the stream wrote more than the cap kept.
func (c *capture) Truncated() bool { return c.total > c.limit }

// outputNotes turns the captured output into report notes: one per stream, plus
// one that says how much was dropped when the cap was reached.
func outputNotes(stdout, stderr *capture) []string {
	streams := []struct {
		name string
		c    *capture
	}{{"stdout", stdout}, {"stderr", stderr}}

	var notes []string
	for _, stream := range streams {
		if text := strings.TrimRight(stream.c.String(), "\n"); text != "" {
			notes = append(notes, stream.name+":\n"+text)
		}
		if stream.c.Truncated() {
			notes = append(notes, fmt.Sprintf("%s truncated: kept the first %s of %s",
				stream.name, bytesx.Format(int64(stream.c.limit)), bytesx.Format(int64(stream.c.total))))
		}
	}
	return notes
}

// exitFailure describes why a finished run is a failure: the script's own exit
// status, or whatever else stopped it, followed by the end of its output.
func exitFailure(waitErr error, stdout, stderr *capture) error {
	var exitErr *exec.ExitError
	switch {
	case errors.As(waitErr, &exitErr) && exitErr.ExitCode() >= 0:
		return fmt.Errorf("script exited with status %d%s", exitErr.ExitCode(), tailSuffix(stdout, stderr))
	case errors.As(waitErr, &exitErr):
		return fmt.Errorf("script was killed by a signal%s", tailSuffix(stdout, stderr))
	default:
		return fmt.Errorf("wait for script: %w%s", waitErr, tailSuffix(stdout, stderr))
	}
}

// tailSuffix is the end of a failed run's output, written as a continuation of a
// failure message.
func tailSuffix(stdout, stderr *capture) string {
	if tail := tailOf(stdout, stderr); tail != "" {
		return "; last output: " + tail
	}
	return ""
}

// tailOf is the end of the output, flattened to one line so a failure stays one
// line in a report and in an alert.
func tailOf(stdout, stderr *capture) string {
	flattened := strings.Join(strings.Fields(stdout.String()+stderr.String()), " ")
	if flattened == "" {
		return ""
	}
	runes := []rune(flattened)
	if len(runes) > maxTailBytes {
		return "…" + string(runes[len(runes)-maxTailBytes:])
	}
	return flattened
}
