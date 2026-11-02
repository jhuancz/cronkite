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
	name string
	dest string
	// sourceLabel is the root as the operator wrote it. Notes name the source and
	// the destination the way the configuration does, because a rehearsal happens
	// before the destination exists: on Windows the resolved form of a directory
	// that does not exist yet is the 8.3 short name, and a note that mixed that
	// with the long form of the source described one run in two spellings.
	sourceLabel string
	policy      snapshot.PrunePolicy
	verify      int
	// maxBytes is the ceiling on the bytes one run may copy. Relinked bytes are
	// not charged against it: they cost no disk, so counting them would refuse
	// the incremental run this task exists to take.
	maxBytes int64
	// force bypasses maxBytes for this run, the way run.safety is bypassed for
	// deletions. It is never silent: a plan that exceeds the ceiling records the
	// bypass in its notes.
	force  bool
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
	maxBytes, err := cfg.BackupBytes()
	if err != nil {
		return nil, fmt.Errorf("task %s: %w", cfg.Name, err)
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

	sourceLabel := cfg.Roots[0]
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
		name:        cfg.Name,
		dest:        cfg.Destination,
		sourceLabel: sourceLabel,
		policy:      snapshot.PrunePolicy{Daily: daily, Weekly: weekly},
		verify:      verify,
		maxBytes:    maxBytes,
		guard:       guard,
		writer:      writer,
	}, nil
}

// Name implements task.Runner.
func (t *Task) Name() string { return t.name }

// Type implements task.Runner.
func (t *Task) Type() string { return config.TypeBackup }

// SetForce records whether this run bypasses the safety ceilings.
//
// The engine passes --force to every task that owns a ceiling of its own, the
// same way it bypasses run.safety for deletions. A task without a ceiling has
// nothing to bypass and does not implement the hook.
func (t *Task) SetForce(force bool) { t.force = force }

// Close releases the source handles.
func (t *Task) Close() error {
	err := t.writer.Close()
	if guardErr := t.guard.Close(); guardErr != nil && err == nil {
		err = guardErr
	}
	return err
}

// Plan reports what a run would do, and changes nothing.
//
// A backup does not plan per file. One run builds one directory and publishes it
// with a single rename, so a list of files would offer an operator an approval
// step that no code path can honour: skipping one file would leave a snapshot
// that is not the tree it claims to restore. The work is therefore reported as
// two aggregate actions - the bytes a run would write and the bytes it would
// reuse - and the file counts behind them as notes.
//
// Nothing here writes: no destination directory is created and no snapshot is
// staged. The figures come from internal/backup's read-only walk, which calls the
// same decisions the writer does, so a rehearsal and the run that follows it
// agree about every file.
func (t *Task) Plan(ctx context.Context, env task.Env) (*task.Plan, error) {
	plan := &task.Plan{}

	rehearsal, err := t.writer.Plan(ctx, env.Now)
	if err != nil {
		return nil, fmt.Errorf("task %s: %w", t.name, err)
	}

	if rehearsal.Copied > 0 {
		plan.Actions = append(plan.Actions, task.Action{
			Path: rehearsal.Snapshot,
			Kind: actionWrite,
			Size: rehearsal.BytesCopied,
		})
	}
	if rehearsal.Relinked > 0 {
		plan.Actions = append(plan.Actions, task.Action{
			Path: rehearsal.Previous,
			Kind: actionRelink,
			Size: rehearsal.BytesRelinked,
		})
	}

	source := t.guard.Roots()[0].Dir()
	for _, s := range rehearsal.Skipped {
		plan.Skipped = append(plan.Skipped, task.Action{
			Path:   filepath.Join(source, filepath.FromSlash(s.Path)),
			Reason: s.Reason,
		})
	}

	// The two aggregate actions carry the bytes; these notes carry the file
	// counts behind them, which is what an operator budgets against.
	plan.Notes = append(plan.Notes, fmt.Sprintf(
		"a run would publish one snapshot of %s in %s, holding %d files (%s): %d to copy (%s), %d to relink (%s)",
		t.sourceLabel, t.dest, rehearsal.Files(), bytesx.Format(rehearsal.BytesCopied+rehearsal.BytesRelinked),
		rehearsal.Copied, bytesx.Format(rehearsal.BytesCopied),
		rehearsal.Relinked, bytesx.Format(rehearsal.BytesRelinked)))
	if rehearsal.Previous == "" {
		plan.Notes = append(plan.Notes, fmt.Sprintf("no previous snapshot in %s: every file would be written", t.dest))
	} else {
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"previous snapshot %s: unchanged files would be hardlinked from it instead of written",
			rehearsal.Previous))
	}
	if len(rehearsal.Skipped) > 0 {
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"%d source entries would be left out: symlinks, special files and unreadable entries are never followed",
			len(rehearsal.Skipped)))
	}

	// A bypass is recorded wherever it happens, exactly like the run.safety
	// ceilings: an operator reading the report must be able to see that the limit
	// was not enforced for this run, and by how much it was missed.
	if t.force && rehearsal.BytesCopied > t.maxBytes {
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"max_backup_bytes (%s) bypassed by --force: this plan would copy %s; the run is not limited by the ceiling",
			bytesx.Format(t.maxBytes), bytesx.Format(rehearsal.BytesCopied)))
	}

	// The ceiling is checked here so a rehearsal refuses instead of letting an
	// operator discover it from a run that wrote nothing.
	if err := t.checkCeiling(rehearsal.BytesCopied); err != nil {
		return nil, err
	}
	return plan, nil
}

// checkCeiling refuses a plan whose bytes to copy exceed max_backup_bytes.
//
// The message names the three things an operator needs to decide what to do:
// what the plan would copy, what the ceiling is, and which setting changes the
// outcome. The error wraps ErrCeiling so the run is refused rather than failed:
// no configuration is broken and nothing was written, so this is a decision for
// a human and not a fault to retry.
func (t *Task) checkCeiling(bytesToCopy int64) error {
	if t.force || bytesToCopy <= t.maxBytes {
		return nil
	}
	return fmt.Errorf(
		"%w: task %s: the plan would copy %s into %s, above max_backup_bytes (%s); nothing was written. Raise max_backup_bytes to cover this copy, or narrow roots so one snapshot is smaller",
		task.ErrCeiling, t.name, bytesx.Format(bytesToCopy), t.dest, bytesx.Format(t.maxBytes),
	)
}

// plannedBytesToCopy is the part of a plan that costs disk: the aggregate write
// action this task's Plan produces. Relinked bytes are deliberately excluded,
// because a hardlink stores nothing new and charging it would refuse exactly the
// runs an incremental backup is meant to make cheap.
func plannedBytesToCopy(plan *task.Plan) int64 {
	if plan == nil {
		return 0
	}
	var total int64
	for _, a := range plan.Actions {
		if a.Kind == actionWrite {
			total += a.Size
		}
	}
	return total
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

	// The ceiling is enforced against the plan that is about to be executed, so a
	// run refuses before it writes anything. Plan has already refused a plan of
	// its own making that was too large; this also covers a plan built by a
	// caller, and it judges the same figure the operator was shown.
	if err := t.checkCeiling(plannedBytesToCopy(plan)); err != nil {
		res.Failed = append(res.Failed, task.Failure{Path: t.dest, Err: err})
		return res, nil
	}

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
