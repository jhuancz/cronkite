package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhuancz/cronkite/internal/fsx"
)

var firstRun = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

func writeSourceFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newWriter(t *testing.T, roots []string, dest string) *Writer {
	t.Helper()
	w, err := NewWriter(guardFor(t, roots...), dest)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// dirNames lists the entries of dir, and nothing if dir does not exist.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func sum(content string) string {
	s := sha256.Sum256([]byte(content))
	return hex.EncodeToString(s[:])
}

func readSnapshotFile(t *testing.T, snapshot, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(snapshot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s from snapshot: %v", rel, err)
	}
	return string(data)
}

func readSnapshotManifest(t *testing.T, snapshot string) *Manifest {
	t.Helper()
	m, err := ReadFile(filepath.Join(snapshot, ManifestName))
	if err != nil {
		t.Fatalf("read snapshot manifest: %v", err)
	}
	return m
}

func sameInode(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	return os.SameFile(ai, bi)
}

func TestWriteCopiesFirstSnapshot(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	writeSourceFile(t, src, "logs/b.log", "bravo!")
	writeSourceFile(t, src, "logs/empty.log", "")
	dest := filepath.Join(t.TempDir(), "backups")

	// A wall clock in another zone must still name the snapshot in UTC.
	res, err := newWriter(t, []string{src}, dest).Write(context.Background(), firstRun.In(time.FixedZone("UTC+2", 7200)))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if want := filepath.Join(dest, "20260301T100000Z"); res.Snapshot != want {
		t.Fatalf("snapshot = %q, want %q", res.Snapshot, want)
	}
	if res.Copied != 3 || res.Relinked != 0 {
		t.Fatalf("copied = %d, relinked = %d, want 3 and 0", res.Copied, res.Relinked)
	}
	if want := int64(len("alpha") + len("bravo!")); res.BytesCopied != want {
		t.Fatalf("bytes copied = %d, want %d", res.BytesCopied, want)
	}
	if res.BytesRelinked != 0 {
		t.Fatalf("bytes deduplicated = %d, want 0", res.BytesRelinked)
	}
	if len(res.Skipped) != 0 || len(res.Fallbacks) != 0 {
		t.Fatalf("skipped = %v, fallbacks = %v, want none", res.Skipped, res.Fallbacks)
	}
	if res.Digest == "" {
		t.Fatal("the result must carry the manifest digest")
	}

	// The snapshot is a plain tree: contents are there, byte for byte.
	if got := readSnapshotFile(t, res.Snapshot, "logs/b.log"); got != "bravo!" {
		t.Fatalf("snapshot content = %q, want %q", got, "bravo!")
	}
	if got := readSnapshotFile(t, res.Snapshot, "a.txt"); got != "alpha" {
		t.Fatalf("snapshot content = %q, want %q", got, "alpha")
	}

	m := readSnapshotManifest(t, res.Snapshot)
	if m.Schema != ManifestSchema {
		t.Fatalf("schema = %d, want %d", m.Schema, ManifestSchema)
	}
	if m.Digest != res.Digest {
		t.Fatalf("manifest digest = %q, want the one in the result %q", m.Digest, res.Digest)
	}
	for rel, content := range map[string]string{"a.txt": "alpha", "logs/b.log": "bravo!", "logs/empty.log": ""} {
		e, ok := m.Lookup(rel)
		if !ok {
			t.Fatalf("manifest is missing %s", rel)
		}
		if e.Size != int64(len(content)) {
			t.Errorf("%s size = %d, want %d", rel, e.Size, len(content))
		}
		if e.SHA256 != sum(content) {
			t.Errorf("%s digest = %q, want %q", rel, e.SHA256, sum(content))
		}
	}

	// Only the finished snapshot is left behind: no working directory survived.
	if names := dirNames(t, dest); len(names) != 1 || names[0] != "20260301T100000Z" {
		t.Fatalf("destination holds %v, want only the snapshot", names)
	}
}

func TestWriteRelinksUnchangedFiles(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	writeSourceFile(t, src, "logs/b.log", "bravo!")
	dest := filepath.Join(t.TempDir(), "backups")
	w := newWriter(t, []string{src}, dest)

	first, err := w.Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}
	second, err := w.Write(context.Background(), firstRun.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}

	if second.Copied != 0 {
		t.Fatalf("copied = %d, want 0: an unchanged tree must not be copied again", second.Copied)
	}
	if second.Relinked != 2 {
		t.Fatalf("relinked = %d, want 2", second.Relinked)
	}
	if second.BytesCopied != 0 {
		t.Fatalf("bytes copied = %d, want 0", second.BytesCopied)
	}
	if want := int64(len("alpha") + len("bravo!")); second.BytesRelinked != want {
		t.Fatalf("bytes deduplicated = %d, want %d", second.BytesRelinked, want)
	}
	if len(second.Fallbacks) != 0 {
		t.Fatalf("fallbacks = %v, want none", second.Fallbacks)
	}

	// Relinking shares the blocks, so the two snapshots hold the same inodes.
	if !sameInode(t, filepath.Join(first.Snapshot, "a.txt"), filepath.Join(second.Snapshot, "a.txt")) {
		t.Fatal("an unchanged file must be hardlinked from the previous snapshot")
	}

	// The second snapshot is complete on its own, and the identical tree gives
	// the identical digest.
	if got := readSnapshotFile(t, second.Snapshot, "logs/b.log"); got != "bravo!" {
		t.Fatalf("snapshot content = %q, want %q", got, "bravo!")
	}
	if first.Digest != second.Digest {
		t.Fatalf("digest = %q and %q, want the same digest for the same tree", first.Digest, second.Digest)
	}
}

func TestWriteCopiesChangedFile(t *testing.T) {
	src := t.TempDir()
	changed := writeSourceFile(t, src, "a.txt", "alpha")
	writeSourceFile(t, src, "b.txt", "bravo")
	dest := filepath.Join(t.TempDir(), "backups")
	w := newWriter(t, []string{src}, dest)

	first, err := w.Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}

	// Same size, different content, and an older mtime: the manifest decides on
	// size and mtime, so this must be copied.
	newContent := "ALPHA"
	if err := os.WriteFile(changed, []byte(newContent), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := firstRun.Add(-48 * time.Hour)
	if err := os.Chtimes(changed, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	second, err := w.Write(context.Background(), firstRun.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if second.Copied != 1 || second.Relinked != 1 {
		t.Fatalf("copied = %d, relinked = %d, want 1 and 1", second.Copied, second.Relinked)
	}
	if want := int64(len(newContent)); second.BytesCopied != want {
		t.Fatalf("bytes copied = %d, want %d", second.BytesCopied, want)
	}

	// The previous snapshot is immutable: copying over a changed file must not
	// reach back into it.
	if got := readSnapshotFile(t, first.Snapshot, "a.txt"); got != "alpha" {
		t.Fatalf("the first snapshot changed to %q, want %q", got, "alpha")
	}
	if got := readSnapshotFile(t, second.Snapshot, "a.txt"); got != newContent {
		t.Fatalf("the second snapshot holds %q, want %q", got, newContent)
	}
	e, ok := readSnapshotManifest(t, second.Snapshot).Lookup("a.txt")
	if !ok || e.SHA256 != sum(newContent) {
		t.Fatalf("manifest record = %+v, %v, want the new content digest", e, ok)
	}
}

// TestWriteRecordsRelinkFallback covers the honest-failure rule: when the
// filesystem refuses a hardlink, the copy happens and is recorded, so a report
// can say a snapshot shares no blocks with the previous one.
func TestWriteRecordsRelinkFallback(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	dest := filepath.Join(t.TempDir(), "backups")
	w := newWriter(t, []string{src}, dest)

	first, err := w.Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}

	w.link = func(_, _ string) error { return errors.New("invalid cross-device link") }
	second, err := w.Write(context.Background(), firstRun.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}

	if second.Relinked != 0 {
		t.Fatalf("relinked = %d, want 0", second.Relinked)
	}
	if second.Copied != 1 {
		t.Fatalf("copied = %d, want 1: a failed link must fall back to a copy", second.Copied)
	}
	if len(second.Fallbacks) != 1 {
		t.Fatalf("fallbacks = %v, want one recorded fallback", second.Fallbacks)
	}
	if second.Fallbacks[0].Path != "a.txt" {
		t.Fatalf("fallback path = %q, want %q", second.Fallbacks[0].Path, "a.txt")
	}
	if !strings.Contains(second.Fallbacks[0].Reason, "cross-device") {
		t.Fatalf("fallback reason = %q, want the link error", second.Fallbacks[0].Reason)
	}
	if want := int64(len("alpha")); second.BytesCopied != want || second.BytesRelinked != 0 {
		t.Fatalf("bytes copied = %d, deduplicated = %d, want %d and 0", second.BytesCopied, second.BytesRelinked, want)
	}

	// The snapshot is still correct; only the accounting says blocks were spent.
	if got := readSnapshotFile(t, second.Snapshot, "a.txt"); got != "alpha" {
		t.Fatalf("snapshot content = %q, want %q", got, "alpha")
	}
	if first.Digest != second.Digest {
		t.Fatalf("digest = %q and %q, want the same digest for the same tree", first.Digest, second.Digest)
	}
}

func TestWriteCopiesWhenPreviousTargetIsGone(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	dest := filepath.Join(t.TempDir(), "backups")
	w := newWriter(t, []string{src}, dest)

	first, err := w.Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := os.Remove(filepath.Join(first.Snapshot, "a.txt")); err != nil {
		t.Fatal(err)
	}

	second, err := w.Write(context.Background(), firstRun.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if second.Copied != 1 || second.Relinked != 0 {
		t.Fatalf("copied = %d, relinked = %d, want 1 and 0", second.Copied, second.Relinked)
	}
	if len(second.Fallbacks) != 0 {
		t.Fatalf("fallbacks = %v, want none: a missing target is not a link failure", second.Fallbacks)
	}
	if got := readSnapshotFile(t, second.Snapshot, "a.txt"); got != "alpha" {
		t.Fatalf("snapshot content = %q, want %q", got, "alpha")
	}
}

func TestWriteSkipsSymlinks(t *testing.T) {
	outside := t.TempDir()
	writeSourceFile(t, outside, "secret.txt", "do not copy")

	src := t.TempDir()
	writeSourceFile(t, src, "keep.txt", "kept")
	if err := os.Symlink(outside, filepath.Join(src, "link-dir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(src, "keep.txt"), filepath.Join(src, "link-file")); err != nil {
		t.Fatal(err)
	}

	res, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups")).Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if res.Copied != 1 {
		t.Fatalf("copied = %d, want 1: only the regular file", res.Copied)
	}
	want := map[string]string{"link-dir": ReasonSymlink, "link-file": ReasonSymlink}
	if len(res.Skipped) != len(want) {
		t.Fatalf("skipped = %v, want %v", res.Skipped, want)
	}
	for _, s := range res.Skipped {
		if want[s.Path] != s.Reason {
			t.Fatalf("skipped %q with reason %q, want %q", s.Path, s.Reason, want[s.Path])
		}
	}

	// Never followed: neither the link nor what it points at is in the snapshot.
	for _, rel := range []string{"link-dir", "link-dir/secret.txt", "link-file"} {
		if _, err := os.Lstat(filepath.Join(res.Snapshot, rel)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s must not appear in the snapshot, lstat error: %v", rel, err)
		}
		if _, ok := readSnapshotManifest(t, res.Snapshot).Lookup(rel); ok {
			t.Fatalf("%s must not be recorded in the manifest", rel)
		}
	}
}

func TestWriteSkipsReservedManifestName(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, ManifestName, "this is a source file, not a manifest")
	dest := filepath.Join(t.TempDir(), "backups")

	res, err := newWriter(t, []string{src}, dest).Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Path != ManifestName || res.Skipped[0].Reason != ReasonReservedName {
		t.Fatalf("skipped = %v, want %s reserved", res.Skipped, ManifestName)
	}

	// The snapshot manifest is a manifest, not a copy of the source file.
	m := readSnapshotManifest(t, res.Snapshot)
	if len(m.Files) != 0 {
		t.Fatalf("files = %+v, want none", m.Files)
	}
	if m.Digest != res.Digest {
		t.Fatalf("manifest digest = %q, want %q", m.Digest, res.Digest)
	}
}

func TestWriteLeavesNoSnapshotWhenCopyFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not stop root from reading the file")
	}

	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	locked := writeSourceFile(t, src, "locked.txt", "secret")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	dest := filepath.Join(t.TempDir(), "backups")

	res, err := newWriter(t, []string{src}, dest).Write(context.Background(), firstRun)
	if err == nil {
		t.Fatal("expected the unreadable file to fail the snapshot")
	}
	if res == nil {
		t.Fatal("a failed run must still report what it had done")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("error = %v, want a wrapped permission error", err)
	}

	// Atomic by construction: no snapshot and no working directory is left,
	// because the tree was never renamed into place.
	if names := dirNames(t, dest); len(names) != 0 {
		t.Fatalf("a failed run left %v behind", names)
	}
	if _, err := os.Stat(filepath.Join(dest, "20260301T100000Z")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no snapshot must exist after a failure, stat error: %v", err)
	}
	if got := readSnapshotFile(t, src, "a.txt"); got != "alpha" {
		t.Fatalf("the source changed during a failed backup: %q", got)
	}
}

func TestWriteRefusesExistingSnapshot(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	dest := filepath.Join(t.TempDir(), "backups")

	// A snapshot for this timestamp already exists: publishing would replace a
	// backup, so the run is refused before anything is written.
	existing := filepath.Join(dest, "20260301T100000Z")
	writeSourceFile(t, existing, "keep.txt", "an earlier snapshot")

	_, err := newWriter(t, []string{src}, dest).Write(context.Background(), firstRun)
	if err == nil {
		t.Fatal("expected an existing snapshot to be refused")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want it to say the snapshot already exists", err)
	}
	if names := dirNames(t, dest); len(names) != 1 || names[0] != "20260301T100000Z" {
		t.Fatalf("destination holds %v, want only the earlier snapshot", names)
	}
	if got := readSnapshotFile(t, existing, "keep.txt"); got != "an earlier snapshot" {
		t.Fatalf("the earlier snapshot was modified: %q", got)
	}
}

func TestWriteStopsOnCancelledContext(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	dest := filepath.Join(t.TempDir(), "backups")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := newWriter(t, []string{src}, dest).Write(ctx, firstRun); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if names := dirNames(t, dest); len(names) != 0 {
		t.Fatalf("a cancelled run left %v behind", names)
	}
}

func TestWriteIgnoresForeignDirectories(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	dest := filepath.Join(t.TempDir(), "backups")
	w := newWriter(t, []string{src}, dest)

	first, err := w.Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}
	// An operator's note and an interrupted build are not snapshots.
	writeSourceFile(t, filepath.Join(dest, "notes"), "todo.txt", "not a snapshot")
	if err := os.MkdirAll(filepath.Join(dest, "20260301T103000Z"), 0o755); err != nil {
		t.Fatal(err)
	}

	second, err := w.Write(context.Background(), firstRun.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if second.Copied != 0 || second.Relinked != 1 {
		t.Fatalf("copied = %d, relinked = %d, want 0 and 1: the newest directory with a manifest is the previous snapshot", second.Copied, second.Relinked)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digest = %q and %q, want the same digest", first.Digest, second.Digest)
	}
}

func TestNewWriterRequiresOneRoot(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "backups")
	g, err := fsx.NewGuard([]string{t.TempDir(), t.TempDir()}, nil)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	if _, err := NewWriter(g, dest); err == nil {
		t.Fatal("one snapshot holds one tree: more than one root must be refused")
	}
}

func TestNewWriterRefusesDestinationInsideSource(t *testing.T) {
	src := t.TempDir()
	tests := map[string]string{
		"direct child":    filepath.Join(src, "backups"),
		"nested child":    filepath.Join(src, "a", "b", "backups"),
		"the root itself": src,
		"uncleaned path":  filepath.Join(src, "..", filepath.Base(src)),
	}
	for name, dest := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewWriter(guardFor(t, src), dest); err == nil {
				t.Fatalf("destination %q inside the source must be refused", dest)
			}
		})
	}
}

func guardFor(t *testing.T, roots ...string) *fsx.Guard {
	t.Helper()
	g, err := fsx.NewGuard(roots, nil)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}
