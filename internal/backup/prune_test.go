package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// at builds a UTC timestamp for a retention case, so a test reads as the days
// it is about instead of a wall of time.Date arguments. March 2026 starts on a
// Sunday, which puts the ISO week boundary between March 1 and March 2.
func at(day, hour int) time.Time {
	return time.Date(2026, 3, day, hour, 0, 0, 0, time.UTC)
}

// snapshotAt writes one real snapshot at the given time, so retention is tested
// against directories and manifests the writer actually produced.
func snapshotAt(t *testing.T, dest string, when time.Time) string {
	t.Helper()
	src := t.TempDir()
	writeSourceFile(t, src, "state.txt", "alpha")
	w := newWriter(t, []string{src}, dest)
	res, err := w.Write(context.Background(), when)
	if err != nil {
		t.Fatalf("write snapshot at %s: %v", when.Format(time.RFC3339), err)
	}
	return res.Snapshot
}

// snapshotName is the directory name of the snapshot taken at when.
func snapshotName(when time.Time) string { return when.UTC().Format(snapshotTimeLayout) }

func snapshotNames(list []Snapshot) []string {
	names := make([]string, 0, len(list))
	for _, s := range list {
		names = append(names, s.Name)
	}
	return names
}

func deletedNames(list []DeletedSnapshot) []string {
	names := make([]string, 0, len(list))
	for _, s := range list {
		names = append(names, s.Name)
	}
	return names
}

func skippedNames(list []SkippedSnapshot) []string {
	names := make([]string, 0, len(list))
	for _, s := range list {
		names = append(names, s.Name)
	}
	return names
}

func requireNames(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !sameStrings(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// A day keeps only its newest snapshot, so five snapshots over four days under a
// three-day policy leave three, and every removed one says why it went.
func TestPruneKeepsNewestOfEachDay(t *testing.T) {
	dest := t.TempDir()
	snapshotAt(t, dest, at(1, 9))
	snapshotAt(t, dest, at(2, 9))
	snapshotAt(t, dest, at(2, 18))
	snapshotAt(t, dest, at(3, 9))
	snapshotAt(t, dest, at(4, 9))

	res, err := Prune(context.Background(), dest, PrunePolicy{Daily: 3})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	requireNames(t, "kept", snapshotNames(res.Kept), []string{
		snapshotName(at(4, 9)),  // newest day
		snapshotName(at(3, 9)),  // day three
		snapshotName(at(2, 18)), // newest of day two
	})
	// Oldest first, so a report reads chronologically.
	requireNames(t, "deleted", deletedNames(res.Deleted), []string{
		snapshotName(at(1, 9)),
		snapshotName(at(2, 9)),
	})
	for _, d := range res.Deleted {
		if d.Reason != PruneReasonOutsideRetention {
			t.Errorf("deleted %s with reason %q, want %q", d.Name, d.Reason, PruneReasonOutsideRetention)
		}
	}
	// dirNames reads the directory in sorted order, which is chronological here.
	requireNames(t, "destination", dirNames(t, dest), []string{
		snapshotName(at(2, 18)), snapshotName(at(3, 9)), snapshotName(at(4, 9)),
	})
}

// The weekly window is independent of the daily one: with the daily rule off,
// only the newest snapshot of each of two ISO weeks survives.
func TestPruneKeepsNewestOfEachWeek(t *testing.T) {
	dest := t.TempDir()
	snapshotAt(t, dest, at(2, 9))
	snapshotAt(t, dest, at(3, 9))
	snapshotAt(t, dest, at(9, 9))
	snapshotAt(t, dest, at(10, 9))
	snapshotAt(t, dest, at(16, 9))

	res, err := Prune(context.Background(), dest, PrunePolicy{Weekly: 2})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	requireNames(t, "kept", snapshotNames(res.Kept), []string{
		snapshotName(at(16, 9)), // week 12
		snapshotName(at(10, 9)), // newest of week 11
	})
	requireNames(t, "deleted", deletedNames(res.Deleted), []string{
		snapshotName(at(2, 9)),
		snapshotName(at(3, 9)),
		snapshotName(at(9, 9)),
	})
}

// Sunday March 1 and Monday March 2 belong to different ISO weeks, so a weekly
// policy that keeps two weeks keeps both. A calendar-week rule would collapse
// them into one bucket and delete one of two adjacent restore points.
func TestPruneTreatsSundayAndMondayAsDifferentWeeks(t *testing.T) {
	dest := t.TempDir()
	snapshotAt(t, dest, at(1, 9))
	snapshotAt(t, dest, at(2, 9))

	res, err := Prune(context.Background(), dest, PrunePolicy{Weekly: 2})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	requireNames(t, "deleted", deletedNames(res.Deleted), nil)
	requireNames(t, "kept", snapshotNames(res.Kept), []string{snapshotName(at(2, 9)), snapshotName(at(1, 9))})
}

// The newest snapshot is the one an operator restores from, so no policy may
// delete it, not even a one-day window over a month of weekly snapshots.
func TestPruneNeverDeletesTheNewestSnapshot(t *testing.T) {
	dest := t.TempDir()
	snapshotAt(t, dest, at(1, 9))
	snapshotAt(t, dest, at(9, 9))
	snapshotAt(t, dest, at(17, 9))

	res, err := Prune(context.Background(), dest, PrunePolicy{Daily: 1})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	requireNames(t, "kept", snapshotNames(res.Kept), []string{snapshotName(at(17, 9))})
	if _, err := os.Stat(filepath.Join(dest, snapshotName(at(17, 9)))); err != nil {
		t.Fatalf("the newest snapshot must still be on disk: %v", err)
	}
}

// A policy that keeps nothing is a delete-everything policy, and it is refused
// before the first snapshot is touched.
func TestPruneRefusesPolicyThatKeepsNothing(t *testing.T) {
	dest := t.TempDir()
	snapshotAt(t, dest, at(1, 9))
	snapshotAt(t, dest, at(2, 9))
	before := dirNames(t, dest)

	for _, policy := range []PrunePolicy{{}, {Daily: -1, Weekly: 4}, {Daily: 7, Weekly: -1}} {
		res, err := Prune(context.Background(), dest, policy)
		if err == nil {
			t.Fatalf("policy %+v must be refused, got %+v", policy, res)
		}
		if res != nil {
			t.Errorf("policy %+v returned a result with an error: %+v", policy, res)
		}
		requireNames(t, "destination after a refused policy", dirNames(t, dest), before)
	}
}

// An index this build cannot read is a reason to look, not a reason to destroy:
// the snapshot is reported and kept while the rest of the policy is applied.
func TestPruneSkipsSnapshotWithUnreadableManifest(t *testing.T) {
	dest := t.TempDir()
	snapshotAt(t, dest, at(1, 9))
	broken := snapshotAt(t, dest, at(2, 9))
	snapshotAt(t, dest, at(3, 9))
	if err := os.WriteFile(filepath.Join(broken, ManifestName), []byte("{not a manifest"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Prune(context.Background(), dest, PrunePolicy{Daily: 1})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	requireNames(t, "kept", snapshotNames(res.Kept), []string{snapshotName(at(3, 9))})
	requireNames(t, "skipped", skippedNames(res.Skipped), []string{snapshotName(at(2, 9))})
	if reason := res.Skipped[0].Reason; reason != ReasonUnreadable {
		t.Errorf("skip reason = %q, want %q", reason, ReasonUnreadable)
	}
	requireNames(t, "deleted", deletedNames(res.Deleted), []string{snapshotName(at(1, 9))})
	if _, err := os.Stat(broken); err != nil {
		t.Fatalf("a snapshot with an unreadable index must survive: %v", err)
	}
}

// If the newest snapshot cannot be read, retention cannot judge the family it
// belongs to, so it prunes nothing at all and says why.
func TestPrunePrunesNothingWhenNewestIsUnreadable(t *testing.T) {
	dest := t.TempDir()
	snapshotAt(t, dest, at(1, 9))
	snapshotAt(t, dest, at(2, 9))
	newest := snapshotAt(t, dest, at(3, 9))
	if err := os.Remove(filepath.Join(newest, ManifestName)); err != nil {
		t.Fatal(err)
	}
	before := dirNames(t, dest)

	res, err := Prune(context.Background(), dest, PrunePolicy{Daily: 1})
	if err == nil {
		t.Fatalf("an unreadable newest snapshot must refuse the pass, got %+v", res)
	}
	requireNames(t, "destination", dirNames(t, dest), before)
	if res == nil || len(res.Deleted) != 0 {
		t.Fatalf("nothing may be deleted, got %+v", res)
	}
	requireNames(t, "skipped", skippedNames(res.Skipped), []string{snapshotName(at(3, 9))})
}

// A destination holds more than snapshots: an operator's note, a file, a tree
// from another tool. None of it is retention's business.
func TestPruneLeavesForeignEntriesAlone(t *testing.T) {
	dest := t.TempDir()
	snapshotAt(t, dest, at(1, 9))
	snapshotAt(t, dest, at(2, 9))
	writeSourceFile(t, dest, "notes.txt", "keep me")
	writeSourceFile(t, dest, filepath.Join("old-plans", "plan.md"), "keep me too")
	// A file that parses as a snapshot name is still not a snapshot directory.
	writeSourceFile(t, dest, snapshotName(at(1, 12)), "not a snapshot")

	res, err := Prune(context.Background(), dest, PrunePolicy{Daily: 1})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	requireNames(t, "deleted", deletedNames(res.Deleted), []string{snapshotName(at(1, 9))})
	requireNames(t, "destination", dirNames(t, dest), []string{
		snapshotName(at(1, 12)), snapshotName(at(2, 9)), "notes.txt", "old-plans",
	})
}

// The listing retention and plan mode share returns snapshot directories only,
// newest first, and calls a missing destination an empty archive.
func TestSnapshotsListsOnlySnapshotsNewestFirst(t *testing.T) {
	dest := t.TempDir()
	snapshotAt(t, dest, at(1, 9))
	snapshotAt(t, dest, at(3, 9))
	writeSourceFile(t, dest, "notes.txt", "not a snapshot")

	got, err := Snapshots(dest)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	requireNames(t, "snapshots", snapshotNames(got), []string{snapshotName(at(3, 9)), snapshotName(at(1, 9))})
	if !got[0].Time.Equal(at(3, 9)) {
		t.Errorf("newest snapshot time = %v, want %v", got[0].Time, at(3, 9))
	}

	missing, err := Snapshots(filepath.Join(dest, "does-not-exist"))
	if err != nil {
		t.Fatalf("a missing destination is an empty archive, not an error: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing destination listed %v", missing)
	}
}

// A destination with no snapshots is a job that has not run yet, not a
// misconfiguration: the pass succeeds and reports nothing.
func TestPruneOnEmptyDestinationDoesNothing(t *testing.T) {
	res, err := Prune(context.Background(), filepath.Join(t.TempDir(), "empty"), PrunePolicy{Daily: 7})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.Kept)+len(res.Deleted)+len(res.Skipped)+len(res.Failed) != 0 {
		t.Fatalf("expected an empty result, got %+v", res)
	}
}

// A cancelled context stops the pass before it deletes anything.
func TestPruneStopsOnCancelledContext(t *testing.T) {
	dest := t.TempDir()
	snapshotAt(t, dest, at(1, 9))
	snapshotAt(t, dest, at(2, 9))
	before := dirNames(t, dest)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Prune(ctx, dest, PrunePolicy{Daily: 1}); err == nil {
		t.Fatal("expected the cancelled context to be reported")
	}
	requireNames(t, "destination", dirNames(t, dest), before)
}

// The existence check behind pruning is about snapshots, not names: a directory
// that wears a timestamp without holding an index is not deleted.
func TestPruneWontDeleteADirectoryWithoutAnIndex(t *testing.T) {
	dest := t.TempDir()
	snapshotAt(t, dest, at(1, 9))
	snapshotAt(t, dest, at(2, 9))
	bare := filepath.Join(dest, snapshotName(at(1, 12)))
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Prune(context.Background(), dest, PrunePolicy{Daily: 1})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	requireNames(t, "skipped", skippedNames(res.Skipped), []string{snapshotName(at(1, 12))})
	if _, statErr := os.Stat(bare); statErr != nil {
		t.Fatalf("a snapshot-shaped directory with no index must survive: %v", statErr)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("unexpected failures: %+v", res.Failed)
	}
}
