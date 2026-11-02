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
