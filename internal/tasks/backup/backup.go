// Package backup implements the "backup" task type: one incremental, verified
// snapshot per run, followed by retention.
//
// It follows the same two-phase shape as the cleanup tasks - plan first, act
// only in apply mode - but the unit of work is different. A cleanup task decides
// about files one at a time; a backup publishes one directory that is complete
// and restorable on its own, so the plan reports the shape of the work and the
// changes happen inside a staging tree that is renamed into place once.
//
// The snapshot itself is the business of internal/backup. This package is the
// task: it validates the configuration, confines the source, sequences writing,
// verification and pruning so a bad run cannot cost a good snapshot, and
// translates the outcome into the report vocabulary the engine understands.
package backup

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	snapshot "github.com/jhuancz/cronkite/internal/backup"
	"github.com/jhuancz/cronkite/internal/bytesx"
	"github.com/jhuancz/cronkite/internal/config"
	"github.com/jhuancz/cronkite/internal/fsx"
	"github.com/jhuancz/cronkite/internal/report"
	"github.com/jhuancz/cronkite/internal/task"
)

// A backup writes files and relinks them, and a report that only knew about
// deletions could not tell the two apart. The kinds are local until the run
// report grows a vocabulary for writing; they are ordinary report.ActionKind
// values either way, so a report stays readable in the meantime.
var (
	actionWrite  = report.ActionWrite
	actionRelink = report.ActionRelink
)

// Task writes one snapshot per run and prunes the older ones by policy.
type Task struct {
	name   string
	dest   string
	policy snapshot.PrunePolicy
	verify int
	guard  *fsx.Guard
	writer *snapshot.Writer
}

// New validates the task configuration, opens its source root and prepares the
// snapshot writer.
//
// Conditions that would turn a run into a silent no-op are refused here: a
// source root that does not exist on this platform, and a destination inside the
// source, which would make every snapshot walk the previous one and grow without
// bound.
func New(cfg config.Task) (*Task, error) {
	if cfg.Type != config.TypeBackup {
		return nil, fmt.Errorf("unknown task type %q", cfg.Type)
	}
	if cfg.Destination == "" {
		return nil, fmt.Errorf("task %s: destination is required for a backup task", cfg.Name)
	}

	guard, err := fsx.NewGuard(cfg.Roots, cfg.Protect)
	if err != nil {
		return nil, fmt.Errorf("task %s: %w", cfg.Name, err)
	}
	switch {
	case len(guard.Missing()) > 0:
		guard.Close()
		return nil, fmt.Errorf("task %s: source root %s does not exist; a snapshot of nothing is not a backup", cfg.Name, strings.Join(guard.Missing(), ", "))
	case len(guard.Roots()) == 0:
		guard.Close()
		return nil, fmt.Errorf("task %s: no source root is available here; a backup needs a source tree", cfg.Name)
	}

	writer, err := snapshot.NewWriter(guard, cfg.Destination)
	if err != nil {
		guard.Close()
		return nil, fmt.Errorf("task %s: %w", cfg.Name, err)
	}

	// The defaults are the ones config validates, so a task built without going
	// through a parsed configuration - a test, an embedding caller - cannot
	// quietly prune with a different policy than the file describes.
	daily, weekly := config.DefaultRetentionDaily, config.DefaultRetentionWeekly
	if cfg.Retention.Daily != nil {
		daily = *cfg.Retention.Daily
	}
	if cfg.Retention.Weekly != nil {
		weekly = *cfg.Retention.Weekly
	}
	verify := config.DefaultSamplePercent
	if cfg.VerifySamplePercent != nil {
		verify = *cfg.VerifySamplePercent
	}

	return &Task{
		name:   cfg.Name,
		dest:   cfg.Destination,
		policy: snapshot.PrunePolicy{Daily: daily, Weekly: weekly},
		verify: verify,
		guard:  guard,
		writer: writer,
	}, nil
}

// Name implements task.Runner.
func (t *Task) Name() string { return t.name }

// Type implements task.Runner.
func (t *Task) Type() string { return config.TypeBackup }

// Close releases the source handles.
func (t *Task) Close() error {
	err := t.writer.Close()
	if guardErr := t.guard.Close(); guardErr != nil && err == nil {
		err = guardErr
	}
	return err
}

// sourceStats is what a plan needs to know about a source tree without reading
// its bytes.
type sourceStats struct {
	files   int
	bytes   int64
	skipped int
}

// Plan reports the shape of the work a run would do, and changes nothing.
//
// A backup's unit of work is the snapshot, not a file, so this plan carries no
// per-file actions: a run creates one directory and publishes it with a single
// rename, and a list of files would offer the operator a choice to approve or
// skip one at a time that no code path can honour. What a run would do is
// reported as notes instead - how many files and bytes it would hold, and
// whether there is a previous snapshot to relink against. Nothing here writes:
// no destination directory is created and no snapshot is staged.
func (t *Task) Plan(ctx context.Context, env task.Env) (*task.Plan, error) {
	plan := &task.Plan{}

	stats, err := t.scan(ctx)
	if err != nil {
		return nil, err
	}
	source := t.guard.Roots()[0].Dir()

	plan.Notes = append(plan.Notes, fmt.Sprintf(
		"a run would publish one snapshot of %s in %s, holding %d files (%s)",
		source, t.dest, stats.files, bytesx.Format(stats.bytes)))

	previous, err := snapshot.Snapshots(t.dest)
	if err != nil {
		return nil, fmt.Errorf("task %s: %w", t.name, err)
	}
	if len(previous) == 0 {
		plan.Notes = append(plan.Notes, fmt.Sprintf("no previous snapshot in %s: every file would be written", t.dest))
	} else {
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"previous snapshot %s exists: unchanged files would be hardlinked from it instead of written",
			previous[0].Path))
	}
	if stats.skipped > 0 {
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"%d source entries would be left out: symlinks, special files and unreadable entries are never followed",
			stats.skipped))
	}
	return plan, nil
}

// scan walks the source through the guard, counting what a snapshot would hold.
//
// It reads names and metadata only: plan mode must be as cheap as looking, and
// hashing the tree to report exact bytes is the work of the run being planned.
func (t *Task) scan(ctx context.Context) (sourceStats, error) {
	var stats sourceStats
	err := t.guard.Roots()[0].Walk(0, func(abs, _ string, d fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			stats.skipped++
			return nil
		}
		info, err := d.Info()
		if err != nil {
			stats.skipped++
			return nil
		}
		stats.files++
		stats.bytes += info.Size()
		return nil
	}, func(_ string, _ error) {
		stats.skipped++
	})
	if err != nil {
		return stats, fmt.Errorf("task %s: walk source %s: %w", t.name, t.guard.Roots()[0].Dir(), err)
	}
	return stats, nil
}

// Apply writes one snapshot, verifies a sample of it against the source, and
// prunes the snapshots the policy no longer keeps.
//
// The order is the safety property. Verification runs before retention, and a
// failed verification prunes nothing: deleting the previous restore points while
// the one just written is unproven would leave an operator with no snapshot they
// can trust. A snapshot that cannot be written at all is an error, because then
// nothing was published and there is nothing to report as done.
func (t *Task) Apply(ctx context.Context, plan *task.Plan, env task.Env) (*task.Result, error) {
	res := &task.Result{}

	written, err := t.writer.Write(ctx, env.Now)
	if err != nil {
		return res, fmt.Errorf("task %s: write snapshot: %w", t.name, err)
	}
	source := t.guard.Roots()[0].Dir()

	// The snapshot is done work as a whole, and each of its files is one action:
	// the report names what was written and what was hardlinked from the previous
	// snapshot, because the difference is what incremental means.
	for _, f := range written.Files {
		kind := actionWrite
		if f.Shared {
			kind = actionRelink
		}
		res.Done = append(res.Done, task.Action{
			Path: filepath.Join(written.Snapshot, filepath.FromSlash(f.Path)),
			Kind: kind,
			Size: f.Size,
		})
	}
	for _, s := range written.Skipped {
		res.Skipped = append(res.Skipped, task.Action{
			Path:   filepath.Join(source, filepath.FromSlash(s.Path)),
			Reason: s.Reason,
		})
	}

	verified, err := snapshot.VerifySnapshot(ctx, written.Snapshot, source, snapshot.VerifyOptions{SamplePercent: t.verify})
	if err != nil {
		return res, fmt.Errorf("task %s: verify snapshot %s: %w", t.name, written.Snapshot, err)
	}
	for _, f := range verified.Failures {
		entry := filepath.Join(verified.Snapshot, filepath.FromSlash(f.Path))
		cause := fmt.Errorf("snapshot entry does not match its manifest (%s)", f.Reason)
		if f.Got != "" {
			cause = fmt.Errorf("snapshot entry does not match its manifest (%s): recorded %s, read %s", f.Reason, f.Want, f.Got)
		}
		res.Failed = append(res.Failed, task.Failure{Path: entry, Err: cause})
	}
	if verified.Failed() {
		// Retention waits for the next successful run: the snapshots it would
		// delete are the last ones an operator could restore from.
		return res, nil
	}

	pruned, err := snapshot.Prune(ctx, t.dest, t.policy)
	if err != nil {
		// The snapshot of this run is written and verified, so a retention pass
		// that could not run is a failure of the task, not a reason to throw the
		// snapshot away - but it is never silent.
		res.Failed = append(res.Failed, task.Failure{Path: t.dest, Err: fmt.Errorf("retention: %w", err)})
		return res, nil
	}
	for _, s := range pruned.Skipped {
		res.Skipped = append(res.Skipped, task.Action{Path: s.Path, Reason: s.Reason})
	}
	for _, d := range pruned.Deleted {
		res.Done = append(res.Done, task.Action{Path: d.Path, Kind: report.ActionDelete, Reason: d.Reason})
	}
	for _, f := range pruned.Failed {
		res.Failed = append(res.Failed, task.Failure{Path: f.Path, Err: f.Err})
	}
	return res, nil
}
