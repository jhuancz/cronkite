package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A snapshot is only restorable if it carries the source timestamps: a tree
// whose files all look freshly written breaks everything that reasons about age,
// and the manifest claims to record the real mtime.
func TestSnapshotPreservesModTime(t *testing.T) {
	src := t.TempDir()
	path := writeSourceFile(t, src, "old.txt", "old")
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	res, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "d")).Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(filepath.Join(res.Snapshot, "old.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// Truncated to the second because filesystem timestamp granularity varies.
	if got := info.ModTime().UTC().Truncate(time.Second); !got.Equal(old) {
		t.Fatalf("snapshot mtime = %v, want %v", got, old)
	}
}

// The relink target must be a regular file. A symlink planted where the previous
// snapshot should hold a file would otherwise pull content from outside the
// configured root into a published snapshot.
func TestRelinkRefusesNonRegularTarget(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "d"))
	first, err := w.Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	previous := filepath.Join(first.Snapshot, "a.txt")
	if err := os.Remove(previous); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, previous); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	second, err := w.Write(context.Background(), firstRun.Add(time.Hour))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	entry := filepath.Join(second.Snapshot, "a.txt")
	info, err := os.Lstat(entry)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("snapshot entry is %v, want a regular file", info.Mode())
	}
	content, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "alpha" {
		t.Fatalf("snapshot content = %q, want the source content %q", content, "alpha")
	}
}

// A source directory named like the snapshot index must not abort the run: it is
// skipped and reported, and the rest of the tree is still backed up.
func TestWriteSkipsReservedNameDirectory(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, filepath.Join(ManifestName, "inner", "x.txt"), "x")
	writeSourceFile(t, src, "keep.txt", "keep")

	res, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "d")).Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("a reserved-name directory must not abort the snapshot: %v", err)
	}

	info, err := os.Stat(filepath.Join(res.Snapshot, ManifestName))
	if err != nil {
		t.Fatalf("the snapshot index is missing: %v", err)
	}
	if info.IsDir() {
		t.Fatal("the snapshot index is a directory")
	}
	if _, err := os.Stat(filepath.Join(res.Snapshot, "keep.txt")); err != nil {
		t.Errorf("the rest of the tree must still be backed up: %v", err)
	}
	if !skippedFor(res, ReasonReservedName) {
		t.Errorf("expected a %s skip, got %+v", ReasonReservedName, res.Skipped)
	}
}

// On macOS and Windows the index name collides case-insensitively. Skipping it
// everywhere keeps the result identical across platforms.
func TestWriteSkipsReservedNameIgnoringCase(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "Manifest.json", "collision")

	res, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "d")).Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !skippedFor(res, ReasonReservedName) {
		t.Fatalf("expected a %s skip, got %+v", ReasonReservedName, res.Skipped)
	}
}

// The finding this test pins: content that changes while size and mtime stay the
// same. The default policy re-reads the source, so the new bytes are copied.
func TestDefaultPolicyDetectsContentChangeWithPreservedMetadata(t *testing.T) {
	src := t.TempDir()
	path := writeSourceFile(t, src, "conf.txt", "version=1\n") // 10 bytes
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	w := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "d"))
	if _, err := w.Write(context.Background(), firstRun); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	// Same length, different bytes, timestamp restored: invisible to size+mtime.
	if err := os.WriteFile(path, []byte("version=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}

	second, err := w.Write(context.Background(), firstRun.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if second.Copied != 1 || second.Relinked != 0 {
		t.Fatalf("copied=%d relinked=%d, want the changed file to be copied", second.Copied, second.Relinked)
	}
	if second.RelinkedUnverified != 0 {
		t.Errorf("the default policy must not report unverified relinks, got %d", second.RelinkedUnverified)
	}
	content, err := os.ReadFile(filepath.Join(second.Snapshot, "conf.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "version=2\n" {
		t.Fatalf("snapshot holds stale bytes %q", content)
	}
}

// The fast policy exists for trees where re-reading everything is too expensive.
// It must never be silent about what it did not check, so its consequence is
// pinned here as a documented trade-off rather than an unknown.
func TestSizeMtimePolicyReportsUnverifiedAndCanCarryStaleBytes(t *testing.T) {
	src := t.TempDir()
	path := writeSourceFile(t, src, "conf.txt", "version=1\n")
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	w := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "d"))
	if err := w.SetRelinkMode(RelinkSizeMtime); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(context.Background(), firstRun); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	if err := os.WriteFile(path, []byte("version=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}

	second, err := w.Write(context.Background(), firstRun.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if second.Relinked != 1 || second.RelinkedUnverified != 1 {
		t.Fatalf("relinked=%d unverified=%d, want the relink counted as unverified", second.Relinked, second.RelinkedUnverified)
	}
	content, err := os.ReadFile(filepath.Join(second.Snapshot, "conf.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "version=1\n" {
		t.Fatalf("the fast policy is expected to reuse the previous bytes, got %q", content)
	}
}

func TestSetRelinkModeRejectsUnknown(t *testing.T) {
	w := newWriter(t, []string{t.TempDir()}, filepath.Join(t.TempDir(), "d"))
	if err := w.SetRelinkMode("hope"); err == nil {
		t.Fatal("expected an error for an unknown mode")
	}
}

// JSON cannot carry invalid UTF-8, and coercing two distinct names into the
// replacement character would produce a manifest this build can never read.
func TestManifestRefusesInvalidUTF8Path(t *testing.T) {
	m := NewManifest()
	err := m.Add(Entry{Path: "log-\xff.txt", Size: 1, ModTime: time.Unix(1, 0).UTC(), SHA256: strings.Repeat("a", 64)})
	if err == nil {
		t.Fatal("expected a path that is not valid UTF-8 to be refused")
	}
}

func TestManifestNormalizesSHA256Case(t *testing.T) {
	m := NewManifest()
	upper := strings.ToUpper(strings.Repeat("ab", 32))
	if err := m.Add(Entry{Path: "a.txt", Size: 1, ModTime: time.Unix(1, 0).UTC(), SHA256: upper}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), ManifestName)
	if err := m.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Files[0].SHA256 != strings.ToLower(upper) {
		t.Fatalf("sha256 = %q, want it stored lower-case", got.Files[0].SHA256)
	}
}

// A manifest carries its own digest so it can be checked instead of trusted.
func TestReadFileRejectsTamperedDigest(t *testing.T) {
	m := NewManifest()
	if err := m.Add(Entry{Path: "a.txt", Size: 1, ModTime: time.Unix(1, 0).UTC(), SHA256: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), ManifestName)
	if err := m.WriteFile(path); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), m.Digest, strings.Repeat("f", 64), 1)
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadFile(path); err == nil {
		t.Fatal("a manifest whose digest does not match its records must be refused")
	}
}

func skippedFor(res *Result, reason string) bool {
	for _, s := range res.Skipped {
		if s.Reason == reason {
			return true
		}
	}
	return false
}

// A file edited inside a published snapshot must never be relinked: the new
// snapshot would inherit bytes that do not match the digest it records.
func TestRelinkRefusesTamperedPreviousFile(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")

	w := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "d"))
	first, err := w.Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	tampered := filepath.Join(first.Snapshot, "a.txt")
	if err := os.WriteFile(tampered, []byte("ALPHA-TAMPERED"), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := w.Write(context.Background(), firstRun.Add(time.Hour))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if second.Relinked != 0 || second.Copied != 1 {
		t.Fatalf("copied=%d relinked=%d, want the tampered file copied again", second.Copied, second.Relinked)
	}
	content, err := os.ReadFile(filepath.Join(second.Snapshot, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "alpha" {
		t.Fatalf("snapshot content = %q, want the source content", content)
	}
}
