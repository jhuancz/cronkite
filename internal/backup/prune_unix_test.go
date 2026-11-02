//go:build unix

package backup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A snapshot holds the modes of the source, so a source with a read-only
// directory produces a read-only snapshot. Retention still has to delete it:
// pruning is about whole snapshots, and a restore that was never written to must
// not pin a snapshot forever.
func TestPruneRemovesSnapshotWithReadOnlyDirectory(t *testing.T) {
	requireNoRoot(t)

	src := t.TempDir()
	writeSourceFile(t, src, filepath.Join("uploads", "photo.jpg"), "bytes")
	locked := filepath.Join(src, "locked")
	writeSourceFile(t, src, filepath.Join("locked", "inner.txt"), "inner")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	// TempDir cleanup cannot descend into a tree the test made read-only.
	t.Cleanup(func() { _ = filepath.WalkDir(src, chmodWritable) })

	dest := t.TempDir()
	t.Cleanup(func() { _ = filepath.WalkDir(dest, chmodWritable) })
	w := newWriter(t, []string{src}, dest)
	first, err := w.Write(context.Background(), at(1, 9))
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if _, err := w.Write(context.Background(), at(2, 9)); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	info, err := os.Stat(filepath.Join(first.Snapshot, "locked"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("expected a read-only snapshot directory, got %v", info.Mode())
	}

	res, err := Prune(context.Background(), dest, PrunePolicy{Daily: 1})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("a read-only tree must be removed after making it writable, got failures %+v", res.Failed)
	}
	requireNames(t, "deleted", deletedNames(res.Deleted), []string{filepath.Base(first.Snapshot)})
	if _, err := os.Stat(first.Snapshot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("snapshot %s is still there: %v", first.Snapshot, err)
	}
}

// A snapshot that cannot be removed is a failure, never a snapshot counted as
// pruned: the report has to match the disk.
func TestPruneReportsSnapshotItCannotRemove(t *testing.T) {
	requireNoRoot(t)

	dest := t.TempDir()
	stale := snapshotAt(t, dest, at(1, 9))
	snapshotAt(t, dest, at(2, 9))
	if err := os.Chmod(dest, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dest, 0o700) })

	res, err := Prune(context.Background(), dest, PrunePolicy{Daily: 1})
	if err != nil {
		t.Fatalf("a snapshot that cannot be removed is a failure, not a pass error: %v", err)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("nothing was removed, but the result claims %v", deletedNames(res.Deleted))
	}
	if len(res.Failed) != 1 || res.Failed[0].Path != stale {
		t.Fatalf("failed = %+v, want exactly %s", res.Failed, stale)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("the snapshot was reported failed but is gone: %v", err)
	}
}

// requireNoRoot skips a permission case that cannot be observed as root, which
// ignores the mode bits it is about.
func requireNoRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not restrict removal")
	}
}

// chmodWritable makes a tree removable again, so the temporary directories of a
// test can be cleaned up after it made part of them read-only.
func chmodWritable(path string, d fs.DirEntry, err error) error {
	if err == nil && d != nil && d.IsDir() {
		_ = os.Chmod(path, 0o700)
	}
	return nil
}
