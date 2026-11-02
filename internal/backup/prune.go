package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Retention is the other half of a snapshot archive.
//
// A writer that only ever adds snapshots fills a disk, and a retention pass that
// guesses is worse than none: the only thing a backup must never do is delete
// the one snapshot an operator needs. So pruning here is deliberately narrow. It
// considers only directories that parse as a snapshot name, it reads each
// snapshot's index before it touches it, it keeps the newest snapshot whatever
// the policy says, and it removes whole snapshots and never a file inside one.

// PruneReasonOutsideRetention is the reason recorded for a snapshot no window
// keeps.
const PruneReasonOutsideRetention = "outside_retention"

// PrunePolicy is how much snapshot history a destination keeps.
//
// Both windows are counted in snapshots of a period, not in calendar age: the
// daily rule keeps the newest snapshot of each UTC day, and the weekly rule the
// newest of each ISO week, for as many distinct days and weeks as the policy
// allows. Counting the days that hold a snapshot rather than the days since the
// newest one means a job that was stopped for a month still keeps its history
// when it resumes, instead of finding that the passage of time alone condemned
// everything but the newest snapshot.
//
// Zero turns a rule off. Both zero is refused, because a retention policy that
// keeps nothing is a delete-everything policy that has not said so.
type PrunePolicy struct {
	Daily  int `json:"daily"`
	Weekly int `json:"weekly"`
}

// validate rejects a policy that cannot be applied, so "off" is always spelled
// by exactly one window and never by an empty policy.
func (p PrunePolicy) validate() error {
	switch {
	case p.Daily < 0:
		return fmt.Errorf("backup: retention daily %d is negative", p.Daily)
	case p.Weekly < 0:
		return fmt.Errorf("backup: retention weekly %d is negative", p.Weekly)
	case p.Daily == 0 && p.Weekly == 0:
		return errors.New("backup: retention keeps no snapshot (daily and weekly are both 0): refusing to prune")
	}
	return nil
}

// Snapshot is one published snapshot directory of a destination.
type Snapshot struct {
	// Name is the directory name: the snapshot's UTC timestamp.
	Name string
	// Path is the snapshot directory itself.
	Path string
	// Time is Name parsed back into the time the snapshot records.
	Time time.Time
}

// Snapshots returns the snapshot directories of dest, newest first.
//
// Only directories whose name is a snapshot timestamp are returned. Anything
// else in the destination belongs to somebody else - an operator's note, a tree
// left by an older build - and retention must never decide the fate of a
// directory it cannot account for. A destination that does not exist yet holds
// no snapshots, which is what the first run of a job sees.
func Snapshots(dest string) ([]Snapshot, error) {
	// The writer resolves its destination so a report never names a path it did
	// not write to; prune has to agree, or a deleted-snapshot path in the report
	// would differ from the one the snapshot was created with.
	dest = resolve(dest)
	abs, err := filepath.Abs(dest)
	if err != nil {
		return nil, fmt.Errorf("resolve destination %q: %w", dest, err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read destination %s: %w", abs, err)
	}

	out := make([]Snapshot, 0, len(entries))
	for _, e := range entries {
		// IsDir is false for a symlink to a directory, which is deliberate: a
		// link is not a snapshot this package wrote, and following one would
		// point retention at a tree outside the destination.
		if !e.IsDir() {
			continue
		}
		at, err := time.Parse(snapshotTimeLayout, e.Name())
		if err != nil {
			continue
		}
		out = append(out, Snapshot{Name: e.Name(), Path: filepath.Join(abs, e.Name()), Time: at.UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// PruneResult reports what one retention pass did.
//
// Every snapshot the pass considered is named in exactly one of the lists, so an
// operator can answer "why is this still here" and "why did that go" from the
// result alone, without re-reading the destination.
type PruneResult struct {
	// Kept are the snapshots retention preserved, newest first.
	Kept []Snapshot
	// Deleted are the snapshots it removed, oldest first, each with the reason.
	Deleted []DeletedSnapshot
	// Skipped are snapshot-shaped directories it refused to judge, because
	// their index could not be read.
	Skipped []SkippedSnapshot
	// Failed are the snapshots it could not remove.
	Failed []PruneFailure
}

// DeletedSnapshot is one removed snapshot and the rule that condemned it.
type DeletedSnapshot struct {
	Snapshot
	// Reason is why it was outside retention.
	Reason string
}

// SkippedSnapshot is one snapshot retention left alone, with the reason.
type SkippedSnapshot struct {
	Snapshot
	// Reason is why it was not judged.
	Reason string
}

// PruneFailure is one snapshot that could not be removed.
type PruneFailure struct {
	// Path is the snapshot directory that is still there.
	Path string
	// Err is what prevented its removal.
	Err error
}

// Prune deletes the snapshots of dest that the policy does not keep.
//
// The returned error means retention did not run: an unusable policy, a
// destination that cannot be read, or a newest snapshot whose index cannot be
// read. Deleting in the last case would be the one failure this package cannot
// recover from - it would remove restore points on the strength of an index the
// tool just admitted it does not understand - so nothing is pruned instead.
//
// A single snapshot that cannot be removed, or one older than the newest whose
// index cannot be read, does not stop the pass: it is reported and the rest of
// the policy is applied.
func Prune(ctx context.Context, dest string, policy PrunePolicy) (*PruneResult, error) {
	if err := policy.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	snapshots, err := Snapshots(dest)
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	res := &PruneResult{}
	if len(snapshots) == 0 {
		// Nothing to prune, and nothing to learn: a job that has not written a
		// snapshot yet is not a misconfiguration.
		return res, nil
	}

	// The newest snapshot is read before anything is deleted. Retention cannot
	// judge the family of a snapshot it cannot account for, and "the newest one is
	// corrupt" is exactly the situation where an operator needs the older ones.
	newest := snapshots[0]
	if err := readable(newest); err != nil {
		res.Skipped = append(res.Skipped, SkippedSnapshot{Snapshot: newest, Reason: ReasonUnreadable})
		return res, fmt.Errorf("backup: newest snapshot %s cannot be read, so nothing was pruned: %w", newest.Path, err)
	}

	keep := keepSet(snapshots, policy)
	for _, s := range snapshots {
		if keep[s.Name] {
			res.Kept = append(res.Kept, s)
		}
	}

	// Oldest first: the snapshot closest to the retention boundary goes last, so
	// a pass that is interrupted leaves the more recent history intact.
	for i := len(snapshots) - 1; i >= 0; i-- {
		s := snapshots[i]
		if keep[s.Name] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		// An index this build cannot read is a reason to look, not a reason to
		// destroy: the directory is reported and stays where it is.
		if err := readable(s); err != nil {
			res.Skipped = append(res.Skipped, SkippedSnapshot{Snapshot: s, Reason: ReasonUnreadable})
			continue
		}
		if err := removeSnapshot(s.Path); err != nil {
			res.Failed = append(res.Failed, PruneFailure{Path: s.Path, Err: err})
			continue
		}
		res.Deleted = append(res.Deleted, DeletedSnapshot{Snapshot: s, Reason: PruneReasonOutsideRetention})
	}
	return res, nil
}

// readable checks that a snapshot carries an index this build can read.
//
// The check is the difference between pruning snapshots and pruning directories:
// the name is only a timestamp, and a directory wearing one without holding a
// complete snapshot is not something a retention pass may delete on the strength
// of its name.
func readable(s Snapshot) error {
	index := filepath.Join(s.Path, ManifestName)
	info, err := os.Lstat(index)
	if err != nil {
		return fmt.Errorf("read snapshot %s index: %w", s.Name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("read snapshot %s index: %s is %v, want a regular file", s.Name, ManifestName, info.Mode())
	}
	// ReadFile checks the records against the digest the manifest carries, so a
	// truncated or edited index is unreadable rather than merely present.
	if _, err := ReadFile(index); err != nil {
		return err
	}
	return nil
}

// keepSet returns the names of the snapshots the policy keeps.
//
// Both windows are crossed in one newest-first pass. The first snapshot of a day
// is that day's newest, and the daily rule keeps it while fewer than Daily days
// have been seen; the weekly rule works the same way over ISO weeks. The two
// windows are independent on purpose: daily answers "what did this look like
// yesterday", weekly "what did it look like last month", and neither needs the
// other to be correct.
func keepSet(snapshots []Snapshot, policy PrunePolicy) map[string]bool {
	keep := make(map[string]bool, len(snapshots))
	// Never the newest, whatever the policy says: a destination with nothing to
	// restore from is not a backup.
	if len(snapshots) > 0 {
		keep[snapshots[0].Name] = true
	}

	days := make(map[string]bool, len(snapshots))
	weeks := make(map[string]bool, len(snapshots))
	for _, s := range snapshots {
		utc := s.Time.UTC()
		if policy.Daily > 0 {
			day := utc.Format("2006-01-02")
			if !days[day] {
				days[day] = true
				if len(days) <= policy.Daily {
					keep[s.Name] = true
				}
			}
		}
		if policy.Weekly > 0 {
			year, week := utc.ISOWeek()
			key := fmt.Sprintf("%04d-W%02d", year, week)
			if !weeks[key] {
				weeks[key] = true
				if len(weeks) <= policy.Weekly {
					keep[s.Name] = true
				}
			}
		}
	}
	return keep
}

// removeSnapshot deletes one whole snapshot directory, including a tree somebody
// left read-only.
//
// os.RemoveAll cannot unlink the entries of a directory that is not writable, so
// a snapshot holding a read-only directory fails on the first attempt. A
// snapshot is deleted as a whole or not at all: the tree is made writable and the
// removal retried once, and a snapshot that still cannot be removed is reported
// as a failure instead of being counted as pruned.
func removeSnapshot(dir string) error {
	err := os.RemoveAll(dir)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrPermission) && !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("remove snapshot %s: %w", dir, err)
	}
	if chmodErr := makeWritable(dir); chmodErr != nil {
		return fmt.Errorf("remove snapshot %s: %w (retry after making the tree writable failed: %v)", dir, err, chmodErr)
	}
	if retryErr := os.RemoveAll(dir); retryErr != nil {
		return fmt.Errorf("remove snapshot %s after making the tree writable: %w", dir, retryErr)
	}
	return nil
}

// makeWritable adds the owner's permission to every directory and file of a
// tree, so it can be removed.
//
// Directories need write and search permission for their entries to be unlinked;
// files need it on Windows, where a read-only attribute refuses removal even
// though the parent directory allows it. A tree that cannot be walked is still
// chmodded as far as it could be reached, and the first failure is returned.
func makeWritable(root string) error {
	var firstErr error
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return nil
		}
		mode := fs.FileMode(0o600)
		if d.IsDir() {
			mode = 0o700
		}
		if chmodErr := os.Chmod(path, mode); chmodErr != nil && firstErr == nil {
			firstErr = chmodErr
		}
		return nil
	})
	if walkErr != nil && firstErr == nil {
		firstErr = walkErr
	}
	return firstErr
}
