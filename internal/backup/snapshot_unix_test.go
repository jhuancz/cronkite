//go:build unix

package backup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestWriteSkipsNonRegularFiles covers devices, sockets and FIFOs. They are
// recorded with a reason instead of being read, matching the cleanup policy:
// cronkite cannot prove what they are, so it does not copy them.
func TestWriteSkipsNonRegularFiles(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "keep.txt", "kept")

	pipe := filepath.Join(src, "app.pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("cannot create a FIFO: %v", err)
	}

	res, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups")).Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if res.Copied != 1 {
		t.Fatalf("copied = %d, want 1", res.Copied)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Path != "app.pipe" || res.Skipped[0].Reason != ReasonNotRegular {
		t.Fatalf("skipped = %v, want app.pipe recorded as %q", res.Skipped, ReasonNotRegular)
	}
	if _, err := os.Lstat(filepath.Join(res.Snapshot, "app.pipe")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the FIFO must not appear in the snapshot, lstat error: %v", err)
	}
	if _, ok := readSnapshotManifest(t, res.Snapshot).Lookup("app.pipe"); ok {
		t.Fatal("the FIFO must not be recorded in the manifest")
	}
}

// A Unix file name may contain a backslash, which the manifest refuses because
// on Windows it would restore as a path separator. Skipping that one name must
// not abort the backup of everything else.
func TestWriteSkipsBackslashName(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, `weird\name.txt`), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSourceFile(t, src, "ok.txt", "ok")

	res, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "d")).Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("one unrepresentable name must not abort the snapshot: %v", err)
	}
	if !skippedFor(res, ReasonNotRepresentable) {
		t.Fatalf("expected a %s skip, got %+v", ReasonNotRepresentable, res.Skipped)
	}
	if _, err := os.Stat(filepath.Join(res.Snapshot, "ok.txt")); err != nil {
		t.Errorf("the rest of the tree must still be backed up: %v", err)
	}
}

// A legal Unix file name that JSON cannot carry must cost that file, not the
// whole snapshot.
func TestWriteSkipsInvalidUTF8Name(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "log-\xff.txt"), []byte("x"), 0o600); err != nil {
		t.Skipf("filesystem refuses non-UTF-8 names: %v", err)
	}
	writeSourceFile(t, src, "ok.txt", "ok")

	res, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "d")).Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("one unrepresentable name must not abort the snapshot: %v", err)
	}
	if !skippedFor(res, ReasonNotRepresentable) {
		t.Fatalf("expected a %s skip, got %+v", ReasonNotRepresentable, res.Skipped)
	}
	if _, err := os.Stat(filepath.Join(res.Snapshot, "ok.txt")); err != nil {
		t.Errorf("the rest of the tree must still be backed up: %v", err)
	}
}

// A symlinked directory inside the previous snapshot would redirect the relink
// target outside it, pulling an unrelated file into the published snapshot.
func TestRelinkRefusesSymlinkedPreviousDirectory(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, filepath.Join("sub", "a.txt"), "alpha")

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "a.txt"), []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "d"))
	first, err := w.Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	previousSub := filepath.Join(first.Snapshot, "sub")
	if err := os.RemoveAll(previousSub); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, previousSub); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	second, err := w.Write(context.Background(), firstRun.Add(time.Hour))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(second.Snapshot, "sub", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "alpha" {
		t.Fatalf("snapshot content = %q, want the source content", content)
	}
}
