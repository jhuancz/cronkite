// Package engine orchestrates a run: select tasks, plan all of them, enforce
// the safety ceilings, then apply.
//
// The ordering is the point. Every plan is computed before the first deletion,
// so a run can be refused in full. There is no path where task two is deleted
// and then the run discovers that task one is ten times over budget.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jhuancz/cronkite/internal/bytesx"
	"github.com/jhuancz/cronkite/internal/config"
	"github.com/jhuancz/cronkite/internal/exitcode"
	"github.com/jhuancz/cronkite/internal/lock"
	"github.com/jhuancz/cronkite/internal/report"
	"github.com/jhuancz/cronkite/internal/task"
	"github.com/jhuancz/cronkite/internal/tasks"
)

// Options controls one run.
type Options struct {
	// ConfigPath is recorded in the report for traceability.
	ConfigPath string
	// Only restricts the run to the named tasks. Empty means every enabled
	// task.
	Only []string
	// Apply turns the plan into changes. When false the run is a rehearsal.
	Apply bool
	// Force bypasses the safety ceilings. The bypass is recorded in the
	// report, never silent.
	Force bool
	// Now is the reference time. Zero means time.Now.
	Now time.Time
}

// job tracks one task through the phases of a run.
type job struct {
	runner    task.Runner
	plan      *task.Plan
	planErr   error
	result    *task.Result
	applyErr  error
	startedAt time.Time
	endedAt   time.Time
	refused   string
}

// Run executes one maintenance run and returns its report. A returned error
// means cronkite could not complete at all; task-level problems live in the
// report.
func Run(ctx context.Context, cfg *config.Config, opts Options) (*report.Report, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	mode := report.ModePlan
	if opts.Apply {
		mode = report.ModeApply
	}

	rep := report.New(mode, opts.ConfigPath)

	selected, err := selectTasks(cfg, opts.Only)
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		rep.Notes = append(rep.Notes, "no enabled task matched the selection; nothing to do")
		finish(rep, exitcode.OK)
		return rep, writeReports(cfg, rep)
	}

	runners := make([]task.Runner, 0, len(selected))
	defer func() {
		for _, r := range runners {
			_ = r.Close()
		}
	}()
	for _, tc := range selected {
		runner, buildErr := tasks.Build(tc)
		if buildErr != nil {
			return nil, buildErr
		}
		runners = append(runners, runner)
	}

	if opts.Apply {
		handle, lockErr := lock.Acquire(cfg.Run.LockFile)
		if lockErr != nil {
			if errors.Is(lockErr, lock.ErrHeld) {
				rep.Notes = append(rep.Notes, "another run holds "+cfg.Run.LockFile+"; this run did nothing")
				rep.Status = report.StatusSkipped
				finish(rep, exitcode.Locked)
				return rep, writeReports(cfg, rep)
			}
			return nil, lockErr
		}
		defer func() { _ = handle.Release() }()
	}

	env := task.Env{Mode: mode, Now: now}
	jobs := make([]*job, 0, len(runners))
	var interrupted error

	// Phase 1: plan everything.
	for _, runner := range runners {
		if err := ctx.Err(); err != nil {
			interrupted = err
			break
		}
		j := &job{runner: runner, startedAt: time.Now()}
		j.plan, j.planErr = runner.Plan(ctx, env)
		j.endedAt = time.Now()
		jobs = append(jobs, j)
	}

	// Phase 2: enforce the ceilings before anything is deleted.
	if opts.Apply {
		if refusal := checkSafety(cfg, jobs, opts.Force); refusal != "" {
			rep.Notes = append(rep.Notes, refusal)
			for _, j := range jobs {
				j.refused = refusal
				rep.Tasks = append(rep.Tasks, taskReport(j, mode, cfg.Report.MaxActionsPerTask))
			}
			rep.Status = report.StatusRefused
			finish(rep, exitcode.Refused)
			return rep, writeReports(cfg, rep)
		}
		if opts.Force {
			rep.Notes = append(rep.Notes, "safety ceilings bypassed (--force); limits were not enforced for this run")
		}
	}

	// Phase 3: apply, only for a real run.
	if opts.Apply {
		for _, j := range jobs {
			if j.plan == nil || j.planErr != nil {
				continue
			}
			if err := ctx.Err(); err != nil {
				interrupted = err
				break
			}
			j.startedAt = time.Now()
			j.result, j.applyErr = j.runner.Apply(ctx, j.plan, env)
			j.endedAt = time.Now()
		}
	}

	for _, j := range jobs {
		rep.Tasks = append(rep.Tasks, taskReport(j, mode, cfg.Report.MaxActionsPerTask))
	}

	if interrupted != nil {
		rep.Notes = append(rep.Notes, "run interrupted: "+interrupted.Error())
	}

	finish(rep, aggregate(rep))
	return rep, writeReports(cfg, rep)
}

// checkSafety returns a refusal message when the planned run exceeds a
// configured ceiling, or "" when it may proceed. Both ceilings exist for the
// same reason: a configuration typo must not wipe a filesystem.
func checkSafety(cfg *config.Config, jobs []*job, force bool) string {
	if force {
		return ""
	}
	var files int
	var bytes int64
	for _, j := range jobs {
		if j.plan == nil {
			continue
		}
		files += j.plan.Files()
		bytes += j.plan.DeleteBytes()
	}
	if files == 0 && bytes == 0 {
		return ""
	}

	if limit := cfg.Run.Safety.MaxDeleteFiles; files > limit {
		return fmt.Sprintf(
			"refusing to run: the plan would remove %d files, above run.safety.max_delete_files (%d); nothing was deleted. Fix the configuration, or re-run with --force if this is intended",
			files, limit)
	}
	if limit := cfg.SafetyBytes(); limit > 0 && bytes > limit {
		return fmt.Sprintf(
			"refusing to run: the plan would remove %s, above run.safety.max_delete_bytes (%s); nothing was deleted. Fix the configuration, or re-run with --force if this is intended",
			bytesx.Format(bytes), bytesx.Format(limit))
	}
	return ""
}

func selectTasks(cfg *config.Config, only []string) ([]config.Task, error) {
	if len(only) == 0 {
		var out []config.Task
		for _, t := range cfg.Tasks {
			if t.IsEnabled() {
				out = append(out, t)
			}
		}
		return out, nil
	}

	known := map[string]config.Task{}
	for _, t := range cfg.Tasks {
		known[t.Name] = t
	}
	seen := map[string]bool{}
	var out []config.Task
	for _, name := range only {
		t, ok := known[name]
		if !ok {
			return nil, fmt.Errorf("unknown task %q (configured: %v)", name, cfg.TaskNames())
		}
		if seen[name] || !t.IsEnabled() {
			continue
		}
		seen[name] = true
		out = append(out, t)
	}
	return out, nil
}

// taskReport converts one job into the report shape.
func taskReport(j *job, mode report.Mode, maxActions int) report.Task {
	rt := report.Task{
		Name:       j.runner.Name(),
		Type:       j.runner.Type(),
		StartedAt:  j.startedAt.UTC(),
		FinishedAt: j.endedAt.UTC(),
		Status:     report.StatusOK,
	}
	rt.DurationMS = rt.FinishedAt.Sub(rt.StartedAt).Milliseconds()

	if j.plan != nil {
		rt.Planned = len(j.plan.Actions)
		rt.PlannedBytes = j.plan.Bytes()
		rt.Notes = append(rt.Notes, j.plan.Notes...)
		rt.Totals.Skipped = len(j.plan.Skipped)
	}
	if j.planErr != nil {
		rt.Status = report.StatusFailed
		rt.Errors = append(rt.Errors, report.Failure{Path: j.runner.Name(), Error: j.planErr.Error()})
		rt.Totals.Errors = 1
		return rt
	}
	if j.refused != "" {
		rt.Status = report.StatusRefused
		return rt
	}

	truncated := false
	add := func(list *[]report.Action, actions []task.Action) {
		for _, a := range actions {
			if len(*list) >= maxActions {
				truncated = true
				return
			}
			*list = append(*list, report.Action{
				Path:   a.Path,
				Action: a.Kind,
				Size:   a.Size,
				Reason: a.Reason,
			})
		}
	}

	switch {
	case mode == report.ModePlan:
		if j.plan != nil {
			add(&rt.Actions, j.plan.Actions)
			add(&rt.Actions, j.plan.Skipped)
		}
	default:
		if j.result != nil {
			add(&rt.Actions, j.result.Done)
			add(&rt.Actions, j.result.Skipped)
			// Each action is counted into the bucket its kind belongs to, so a
			// backup writes and a cleanup frees; neither can be reported as the
			// other. The task's own dir count stays authoritative.
			for _, done := range j.result.Done {
				rt.Totals = rt.Totals.WithAction(done.Kind, done.Size)
			}
			rt.Totals.DirsRemoved = j.result.DirsRemoved
			rt.Totals.Skipped += len(j.result.Skipped)
			for _, f := range j.result.Failed {
				rt.Errors = append(rt.Errors, report.Failure{Path: f.Path, Error: f.Err.Error()})
			}
			rt.Totals.Errors = len(j.result.Failed)
		}
	}
	rt.ActionsTruncated = truncated

	if j.applyErr != nil {
		rt.Status = report.StatusFailed
		rt.Errors = append(rt.Errors, report.Failure{Path: j.runner.Name(), Error: j.applyErr.Error()})
		rt.Totals.Errors++
	} else if rt.Totals.Errors > 0 {
		rt.Status = report.StatusPartial
	}
	return rt
}

// aggregate rolls task outcomes into the run totals and exit code.
func aggregate(rep *report.Report) int {
	code := exitcode.OK
	for _, t := range rep.Tasks {
		rep.Totals = rep.Totals.Add(t.Totals)
		switch t.Status {
		case report.StatusRefused:
			rep.Status = report.StatusRefused
			code = exitcode.Refused
		case report.StatusFailed, report.StatusPartial:
			rep.Status = report.StatusPartial
			if code == exitcode.OK {
				code = exitcode.Partial
			}
		}
	}
	return code
}

func finish(rep *report.Report, code int) {
	rep.Close(time.Now())
	rep.ExitCode = code
}

func writeReports(cfg *config.Config, rep *report.Report) error {
	dir := cfg.Report.Dir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	if err := rep.WriteFile(filepath.Join(dir, "run-"+rep.RunID+".json")); err != nil {
		return err
	}
	return rep.WriteFile(filepath.Join(dir, "latest.json"))
}
