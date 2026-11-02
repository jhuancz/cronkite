package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A snapshot the writer just published, with every byte where the manifest says
// it is, verifies without a failure. The counts still describe a sample, not the
// whole tree: three entries at the default ten percent is one file.
func TestVerifyPassesOnFreshSnapshot(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	writeSourceFile(t, src, "logs/b.log", "bravo!")
	writeSourceFile(t, src, "logs/empty.log", "")

	written, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups")).Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := VerifySnapshot(context.Background(), written.Snapshot, src, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifySnapshot: %v", err)
	}
	if got.Failed() {
		t.Fatalf("failures = %+v, want none", got.Failures)
	}
	if got.Entries != 3 || got.Sampled != 1 || got.PublishedVerified != 1 {
		t.Fatalf("entries = %d, sampled = %d, published verified = %d, want 3, 1 and 1",
			got.Entries, got.Sampled, got.PublishedVerified)
	}
	if got.SamplePercent != DefaultVerifySamplePercent {
		t.Fatalf("sample percent = %d, want the default %d", got.SamplePercent, DefaultVerifySamplePercent)
	}
	// Nothing was shared with a previous snapshot, so no source claim was
	// inherited and nothing pretends to have been checked against the source.
	if got.Relinked != 0 || got.SourceVerified != 0 || got.SourceUnchecked != 0 {
		t.Fatalf("relinked = %d, source verified = %d, unchecked = %d, want zeroes",
			got.Relinked, got.SourceVerified, got.SourceUnchecked)
	}
	if got.Digest != written.Digest {
		t.Fatalf("digest = %q, want the snapshot digest %q", got.Digest, written.Digest)
	}
}

// The explicit "verify everything" mode re-reads the whole manifest.
func TestVerifyAllRehashesEveryEntry(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	writeSourceFile(t, src, "logs/b.log", "bravo!")
	writeSourceFile(t, src, "logs/empty.log", "")

	written, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups")).Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := VerifySnapshot(context.Background(), written.Snapshot, src, VerifyOptions{All: true})
	if err != nil {
		t.Fatalf("VerifySnapshot: %v", err)
	}
	if got.Failed() {
		t.Fatalf("failures = %+v, want none", got.Failures)
	}
	if !got.All || got.Sampled != 3 || got.PublishedVerified != 3 {
		t.Fatalf("all = %v, sampled = %d, published verified = %d, want true, 3 and 3",
			got.All, got.Sampled, got.PublishedVerified)
	}
}

// Published bytes that do not match the manifest are a failure that names the
// path, and never a panic or a silent pass.
func TestVerifyFailsOnDamagedSnapshotEntry(t *testing.T) {
	tests := []struct {
		name       string
		damage     func(t *testing.T, published string)
		wantReason string
		wantGot    string
	}{
		{
			name: "content altered",
			damage: func(t *testing.T, published string) {
				t.Helper()
				if err := os.WriteFile(published, []byte("ALPHA"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonVerifyMismatch,
			wantGot:    sum("ALPHA"),
		},
		{
			name: "entry removed",
			damage: func(t *testing.T, published string) {
				t.Helper()
				if err := os.Remove(published); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonUnreadable,
		},
		{
			name: "entry replaced by a directory",
			damage: func(t *testing.T, published string) {
				t.Helper()
				if err := os.Remove(published); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(published, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonNotRegular,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := t.TempDir()
			writeSourceFile(t, src, "a.txt", "alpha")

			written, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups")).Write(context.Background(), firstRun)
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			tt.damage(t, filepath.Join(written.Snapshot, "a.txt"))

			got, err := VerifySnapshot(context.Background(), written.Snapshot, src, VerifyOptions{All: true})
			if err != nil {
				t.Fatalf("VerifySnapshot: %v", err)
			}
			if len(got.Failures) != 1 {
				t.Fatalf("failures = %+v, want exactly one", got.Failures)
			}
			f := got.Failures[0]
			if f.Path != "a.txt" || f.Reason != tt.wantReason {
				t.Fatalf("failure = %+v, want path %q and reason %q", f, "a.txt", tt.wantReason)
			}
			if f.Want != sum("alpha") {
				t.Fatalf("failure recorded digest = %q, want the manifest digest %q", f.Want, sum("alpha"))
			}
			if f.Got != tt.wantGot {
				t.Fatalf("failure actual digest = %q, want %q", f.Got, tt.wantGot)
			}
			if got.PublishedVerified != 0 {
				t.Fatalf("published verified = %d, want 0: a failed entry is not verified coverage", got.PublishedVerified)
			}
		})
	}
}

// A symlink planted where the snapshot should hold a file is a failure, even
// when its target happens to hold the recorded bytes: following it would mean
// reading outside the snapshot and calling the result verified.
func TestVerifyFailsOnSymlinkedSnapshotEntry(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}

	written, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups")).Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	published := filepath.Join(written.Snapshot, "a.txt")
	if err := os.Remove(published); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, published); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := VerifySnapshot(context.Background(), written.Snapshot, src, VerifyOptions{All: true})
	if err != nil {
		t.Fatalf("VerifySnapshot: %v", err)
	}
	if len(got.Failures) != 1 {
		t.Fatalf("failures = %+v, want exactly one", got.Failures)
	}
	if f := got.Failures[0]; f.Path != "a.txt" || f.Reason != ReasonSymlink {
		t.Fatalf("failure = %+v, want path %q and reason %q", f, "a.txt", ReasonSymlink)
	}
	if got.PublishedVerified != 0 {
		t.Fatalf("published verified = %d, want 0: a symlink is not a verified entry", got.PublishedVerified)
	}
	// The bytes behind the link were never hashed into a pass.
	if data, err := os.ReadFile(outside); err != nil || string(data) != "alpha" {
		t.Fatalf("the file outside the snapshot was touched: %q, %v", data, err)
	}
}

// The finding this test pins: a relinked entry inherits its digest, so only a
// re-read of the source can catch content that changed while size and mtime
// stayed the same. The published bytes are still correct here, which is exactly
// why the published check alone would pass.
func TestVerifyFailsWhenSourceChangedButSizeAndModTimeMatch(t *testing.T) {
	src := t.TempDir()
	path := writeSourceFile(t, src, "conf.txt", "version=1\n")
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	w := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups"))
	first, err := w.Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}
	second, err := w.Write(context.Background(), firstRun.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if second.Relinked != 1 {
		t.Fatalf("relinked = %d, want 1: this test needs an inherited digest", second.Relinked)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digest = %q and %q, want the same tree", first.Digest, second.Digest)
	}

	// Same length, different bytes, timestamp restored: invisible to size+mtime.
	if err := os.WriteFile(path, []byte("version=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != original.Size() || !info.ModTime().Equal(original.ModTime()) {
		t.Fatalf("test setup: size %d, mtime %v, want the originals", info.Size(), info.ModTime())
	}

	got, err := VerifySnapshot(context.Background(), second.Snapshot, src, VerifyOptions{All: true})
	if err != nil {
		t.Fatalf("VerifySnapshot: %v", err)
	}
	if len(got.Failures) != 1 {
		t.Fatalf("failures = %+v, want exactly one", got.Failures)
	}
	f := got.Failures[0]
	if f.Path != "conf.txt" || f.Reason != ReasonSourceMismatch {
		t.Fatalf("failure = %+v, want path %q and reason %q", f, "conf.txt", ReasonSourceMismatch)
	}
	if f.Want != sum("version=1\n") || f.Got != sum("version=2\n") {
		t.Fatalf("failure recorded %q and observed %q, want the inherited digest and the new bytes", f.Want, f.Got)
	}
	// The count says what it checked: the published copy still matches, and the
	// inherited claim was the one that failed.
	if got.Relinked != 1 || got.PublishedVerified != 1 || got.SourceVerified != 0 {
		t.Fatalf("relinked = %d, published verified = %d, source verified = %d, want 1, 1 and 0",
			got.Relinked, got.PublishedVerified, got.SourceVerified)
	}
}

// A source entry that is no longer usable cannot prove an inherited digest.
func TestVerifyFailsOnUnusableSourceForRelinkedEntry(t *testing.T) {
	tests := []struct {
		name       string
		damage     func(t *testing.T, path string)
		wantReason string
	}{
		{
			name: "source removed",
			damage: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonSourceUnreadable,
		},
		{
			name: "source replaced by a directory",
			damage: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonSourceNotRegular,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := t.TempDir()
			path := writeSourceFile(t, src, "a.txt", "alpha")

			w := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups"))
			if _, err := w.Write(context.Background(), firstRun); err != nil {
				t.Fatalf("first Write: %v", err)
			}
			second, err := w.Write(context.Background(), firstRun.Add(time.Hour))
			if err != nil {
				t.Fatalf("second Write: %v", err)
			}
			if second.Relinked != 1 {
				t.Fatalf("relinked = %d, want 1", second.Relinked)
			}
			tt.damage(t, path)

			got, err := VerifySnapshot(context.Background(), second.Snapshot, src, VerifyOptions{All: true})
			if err != nil {
				t.Fatalf("VerifySnapshot: %v", err)
			}
			if len(got.Failures) != 1 {
				t.Fatalf("failures = %+v, want exactly one", got.Failures)
			}
			if f := got.Failures[0]; f.Path != "a.txt" || f.Reason != tt.wantReason {
				t.Fatalf("failure = %+v, want path %q and reason %q", f, "a.txt", tt.wantReason)
			}
			if got.Relinked != 1 || got.SourceVerified != 0 {
				t.Fatalf("relinked = %d, source verified = %d, want 1 and 0", got.Relinked, got.SourceVerified)
			}
		})
	}
}

// Without a source handle the source claim of a relinked entry cannot be
// checked, and the result says so instead of counting it as covered.
func TestVerifyWithoutSourceHandleCountsUncheckedRelinks(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	writeSourceFile(t, src, "logs/b.log", "bravo!")

	w := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups"))
	if _, err := w.Write(context.Background(), firstRun); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	second, err := w.Write(context.Background(), firstRun.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}

	got, err := VerifySnapshot(context.Background(), second.Snapshot, "", VerifyOptions{All: true})
	if err != nil {
		t.Fatalf("VerifySnapshot: %v", err)
	}
	if got.Failed() {
		t.Fatalf("failures = %+v, want none: the published bytes are intact", got.Failures)
	}
	if got.PublishedVerified != 2 {
		t.Fatalf("published verified = %d, want 2", got.PublishedVerified)
	}
	if got.Relinked != 2 || got.SourceVerified != 0 || got.SourceUnchecked != 2 {
		t.Fatalf("relinked = %d, source verified = %d, unchecked = %d, want 2, 0 and 2",
			got.Relinked, got.SourceVerified, got.SourceUnchecked)
	}
}

// The sample is derived from the manifest digest, so a rerun verifies the same
// files and two snapshots of an identical tree sample the same entries.
func TestVerifySampleIsDeterministicForAManifest(t *testing.T) {
	src := t.TempDir()
	const files = 40
	names := make([]string, files)
	for i := range names {
		names[i] = fmt.Sprintf("f%02d.txt", i)
		writeSourceFile(t, src, names[i], fmt.Sprintf("content-%d", i))
	}

	first, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups-a")).Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}
	second, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups-b")).Write(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digest = %q and %q, want the same tree", first.Digest, second.Digest)
	}

	// Tampering every file turns the failure list into the sampled set, which is
	// the only way to observe it through the public result. Removing the file
	// first keeps the hardlink-free snapshots independent.
	tamper := func(t *testing.T, snapshot string) {
		t.Helper()
		for _, name := range names {
			published := filepath.Join(snapshot, name)
			if err := os.Remove(published); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(published, []byte("tampered"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	tamper(t, first.Snapshot)
	tamper(t, second.Snapshot)

	verify := func(t *testing.T, snapshot string) *VerifyResult {
		t.Helper()
		res, err := VerifySnapshot(context.Background(), snapshot, "", VerifyOptions{})
		if err != nil {
			t.Fatalf("VerifySnapshot: %v", err)
		}
		if res.Sampled != files/10 {
			t.Fatalf("sampled = %d, want %d", res.Sampled, files/10)
		}
		if len(res.Failures) != res.Sampled {
			t.Fatalf("failures = %d, want one per sampled entry (%d)", len(res.Failures), res.Sampled)
		}
		return res
	}

	one := verify(t, first.Snapshot)
	again := verify(t, first.Snapshot)
	other := verify(t, second.Snapshot)

	if got, want := failurePaths(other), failurePaths(one); !sameStrings(got, want) {
		t.Fatalf("an identical manifest sampled %v, want the same entries as %v", got, want)
	}
	if got, want := failurePaths(again), failurePaths(one); !sameStrings(got, want) {
		t.Fatalf("a rerun sampled %v, want the same entries as %v", got, want)
	}
}

// Sampling sizes, including the minimum of one entry and the explicit
// everything mode. Reachable only through a real snapshot, so the table builds
// one per case.
func TestVerifySampleSize(t *testing.T) {
	tests := []struct {
		name        string
		files       int
		opts        VerifyOptions
		wantSampled int
		wantPercent int
	}{
		{name: "empty snapshot", files: 0, opts: VerifyOptions{}, wantSampled: 0, wantPercent: DefaultVerifySamplePercent},
		{name: "one entry is always sampled", files: 1, opts: VerifyOptions{}, wantSampled: 1, wantPercent: DefaultVerifySamplePercent},
		{name: "small default sample rounds up to one", files: 3, opts: VerifyOptions{}, wantSampled: 1, wantPercent: DefaultVerifySamplePercent},
		{name: "ten percent of forty", files: 40, opts: VerifyOptions{}, wantSampled: 4, wantPercent: DefaultVerifySamplePercent},
		{name: "half of seven rounds up", files: 7, opts: VerifyOptions{SamplePercent: 50}, wantSampled: 4, wantPercent: 50},
		{name: "one percent of one hundred", files: 100, opts: VerifyOptions{SamplePercent: 1}, wantSampled: 1, wantPercent: 1},
		{name: "hundred percent is everything", files: 5, opts: VerifyOptions{SamplePercent: 100}, wantSampled: 5, wantPercent: 100},
		{name: "explicit everything", files: 5, opts: VerifyOptions{All: true}, wantSampled: 5, wantPercent: 100},
		{name: "everything wins over a percent", files: 5, opts: VerifyOptions{SamplePercent: 10, All: true}, wantSampled: 5, wantPercent: 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := t.TempDir()
			for i := 0; i < tt.files; i++ {
				writeSourceFile(t, src, fmt.Sprintf("f%03d.txt", i), fmt.Sprintf("content-%d", i))
			}
			written, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups")).Write(context.Background(), firstRun)
			if err != nil {
				t.Fatalf("Write: %v", err)
			}

			got, err := VerifySnapshot(context.Background(), written.Snapshot, "", tt.opts)
			if err != nil {
				t.Fatalf("VerifySnapshot: %v", err)
			}
			if got.Failed() {
				t.Fatalf("failures = %+v, want none", got.Failures)
			}
			if got.Entries != tt.files {
				t.Fatalf("entries = %d, want %d", got.Entries, tt.files)
			}
			if got.Sampled != tt.wantSampled {
				t.Fatalf("sampled = %d, want %d", got.Sampled, tt.wantSampled)
			}
			if got.SamplePercent != tt.wantPercent {
				t.Fatalf("sample percent = %d, want %d", got.SamplePercent, tt.wantPercent)
			}
			if got.PublishedVerified != tt.wantSampled {
				t.Fatalf("published verified = %d, want %d: a clean snapshot verifies every sampled entry",
					got.PublishedVerified, tt.wantSampled)
			}
		})
	}
}

func TestVerifyRejectsSamplePercentOutOfRange(t *testing.T) {
	tests := map[string]int{
		"negative":          -1,
		"above one hundred": 101,
	}
	for name, percent := range tests {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			writeSourceFile(t, src, "a.txt", "alpha")
			written, err := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups")).Write(context.Background(), firstRun)
			if err != nil {
				t.Fatalf("Write: %v", err)
			}

			if _, err := VerifySnapshot(context.Background(), written.Snapshot, src, VerifyOptions{SamplePercent: percent}); err == nil {
				t.Fatalf("sample percent %d must be refused, not clamped", percent)
			}
		})
	}
}

func failurePaths(res *VerifyResult) []string {
	paths := make([]string, 0, len(res.Failures))
	for _, f := range res.Failures {
		paths = append(paths, f.Path)
	}
	return paths
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
