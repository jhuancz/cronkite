package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Verification re-reads a published snapshot instead of trusting the record it
// wrote for itself.
//
// A manifest can be wrong in two ways that no amount of care during the write
// catches. Published bytes can drift from the record, because a hardlink shares
// an inode: a file edited or corrupted inside an older snapshot is also edited
// in every snapshot that relinked it, while the new manifest recorded the old
// digest. And a relinked entry's digest is inherited rather than observed, so
// nothing in the snapshot proves it was ever true.
//
// So VerifySnapshot does both: it re-hashes the published file of every sampled
// entry, and it re-hashes the *source* of every entry the writer shared with the
// previous snapshot. Both reads go through an os.Root, so a symlink planted
// inside a snapshot or a source tree cannot make verification read - or bless -
// something outside it.
//
// Verification reports coverage as it is, never as it was hoped to be: the
// result records how many entries were sampled, how many were re-hashed against
// the source, and how many inherited claims could not be checked at all.

// DefaultVerifySamplePercent is the share of the recorded files whose published
// bytes are re-hashed when the caller does not choose.
const DefaultVerifySamplePercent = 10

// Reasons a snapshot entry fails verification. They extend the skip vocabulary
// of a snapshot with the two sides a digest can be wrong on.
const (
	// ReasonVerifyMismatch marks published bytes that do not hash to the digest
	// the manifest records for them.
	ReasonVerifyMismatch = "digest_mismatch"
	// ReasonSourceMismatch marks a relinked entry whose source no longer hashes
	// to the digest inherited from the previous snapshot. It is the only check
	// that catches content changed while size and mtime stayed the same.
	ReasonSourceMismatch = "source_digest_mismatch"
	// ReasonSourceNotRegular marks a source entry that is no longer a regular
	// file: the bytes the manifest inherited cannot be proven from it.
	ReasonSourceNotRegular = "source_not_a_regular_file"
	// ReasonSourceUnreadable marks a source entry that could not be read, which
	// includes one that has since been removed.
	ReasonSourceUnreadable = "source_unreadable"
)

// VerifyOptions selects how much of a snapshot VerifySnapshot re-reads.
type VerifyOptions struct {
	// SamplePercent is the percentage of the recorded files whose published
	// bytes are re-hashed. Zero means DefaultVerifySamplePercent; a negative
	// value or one above 100 is an error rather than a silent clamp.
	SamplePercent int
	// All re-reads every recorded file. It is the explicit "verify everything"
	// mode and it wins over SamplePercent.
	All bool
}

// resolve checks the options and returns the sample that will actually be used.
func (o VerifyOptions) resolve() (all bool, percent int, err error) {
	percent = o.SamplePercent
	if percent == 0 {
		percent = DefaultVerifySamplePercent
	}
	if percent < 0 || percent > 100 {
		return false, 0, fmt.Errorf("verify: sample percent %d is outside 0..100", o.SamplePercent)
	}
	if o.All {
		// Reported as a full sample, because that is what was read.
		return true, 100, nil
	}
	return false, percent, nil
}

// VerifyFailure is one file whose bytes did not match the manifest, or could not
// be read at all.
//
// Failures are values. A mismatch is evidence for the caller to judge, not a
// panic and not an error that aborts the rest of the check.
type VerifyFailure struct {
	// Path is relative to the snapshot root and slash-separated.
	Path string `json:"path"`
	// Reason is one of the Reason* constants of this package.
	Reason string `json:"reason"`
	// Want is the digest recorded in the manifest.
	Want string `json:"want,omitempty"`
	// Got is the digest actually read, empty when nothing could be read.
	Got string `json:"got,omitempty"`
}

// VerifyResult reports what verification actually covered.
//
// The counts are the point of the struct: a run that re-hashed four of two
// thousand files says so, and nothing here can be read as a stronger claim than
// the evidence behind it.
type VerifyResult struct {
	// Snapshot is the verified snapshot directory.
	Snapshot string `json:"snapshot"`
	// Digest is the aggregate manifest digest of the snapshot.
	Digest string `json:"manifest_digest"`
	// Entries is the number of files recorded in the manifest.
	Entries int `json:"entries"`
	// SamplePercent is the share of entries that was selected for re-hashing,
	// after defaults. It is 100 in the verify-everything mode.
	SamplePercent int `json:"verify_sample_percent"`
	// All reports whether the verify-everything mode was used.
	All bool `json:"verify_all,omitempty"`
	// Sampled is the number of entries selected for verification.
	Sampled int `json:"entries_sampled"`
	// PublishedVerified is the number of sampled entries whose published bytes
	// were re-hashed and matched the manifest.
	PublishedVerified int `json:"entries_published_verified"`
	// Relinked is the number of entries the writer shared with the previous
	// snapshot instead of copying, which is exactly the set whose manifest
	// digest was inherited rather than observed.
	Relinked int `json:"entries_relinked"`
	// SourceVerified is the number of relinked entries whose source bytes were
	// re-hashed and matched the inherited digest.
	SourceVerified int `json:"entries_source_verified"`
	// SourceUnchecked is the number of relinked entries that could not be
	// compared with the source because no source handle was supplied. It exists
	// so an inherited claim is never counted as a checked one.
	SourceUnchecked int `json:"entries_source_unchecked,omitempty"`
	// Failures are the entries that did not match, in manifest order.
	Failures []VerifyFailure `json:"failures,omitempty"`
}

// Failed reports whether verification found anything wrong. The caller decides
// what that means for the run.
func (r *VerifyResult) Failed() bool { return r != nil && len(r.Failures) > 0 }

// fail records one mismatch or unreadable entry.
func (r *VerifyResult) fail(path, reason, want, got string) {
	r.Failures = append(r.Failures, VerifyFailure{Path: path, Reason: reason, Want: want, Got: got})
}

// VerifySnapshot checks a published snapshot against its own manifest.
//
// snapshotDir is a snapshot directory, sourceDir is the root the snapshot was
// taken from and may be empty. Without a source the inherited digest of a
// relinked entry cannot be proven, so those entries are counted in
// SourceUnchecked rather than treated as verified.
//
// A returned error means verification could not be performed - the snapshot has
// no readable manifest, a handle could not be opened, the context was cancelled.
// A file that does not match is a VerifyFailure in the result, and the check
// continues with the rest, so one bad file never hides the others.
func VerifySnapshot(ctx context.Context, snapshotDir, sourceDir string, opts VerifyOptions) (*VerifyResult, error) {
	all, percent, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	abs, err := filepath.Abs(snapshotDir)
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot %q: %w", snapshotDir, err)
	}
	// The snapshot is entered through a root handle and never through plain
	// paths: a symlink planted inside it cannot redirect a read outside it.
	snapshot, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open snapshot %s: %w", abs, err)
	}
	defer snapshot.Close()

	// The index itself is opened through the root first, so a symlink under that
	// name is refused, and then read with the package reader, so its records and
	// its own digest are checked before anything is compared with them.
	info, err := snapshot.Lstat(ManifestName)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s has no readable %s: %w", abs, ManifestName, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("snapshot %s: %s is %v, want a regular file", abs, ManifestName, info.Mode())
	}
	manifest, err := ReadFile(filepath.Join(abs, ManifestName))
	if err != nil {
		return nil, err
	}

	res := &VerifyResult{
		Snapshot:      abs,
		Digest:        manifest.Digest,
		Entries:       len(manifest.Files),
		SamplePercent: percent,
		All:           all,
	}

	var source *os.Root
	if sourceDir != "" {
		srcAbs, err := filepath.Abs(sourceDir)
		if err != nil {
			return res, fmt.Errorf("resolve source %q: %w", sourceDir, err)
		}
		source, err = os.OpenRoot(srcAbs)
		if err != nil {
			return res, fmt.Errorf("open source %s for verification: %w", srcAbs, err)
		}
		defer source.Close()
	}

	// Which entries the writer shared instead of copying is not written into the
	// manifest, and it does not need to be: a relinked file *is* the file of the
	// previous snapshot, so the shared inode is the record. Without a previous
	// snapshot nothing could have been relinked.
	previous, err := openPreviousSnapshot(abs)
	if err != nil {
		return res, err
	}
	if previous != nil {
		defer previous.Close()
	}

	sampled := make(map[string]struct{}, len(manifest.Files))
	for _, e := range sampleEntries(manifest, all, percent) {
		sampled[e.Path] = struct{}{}
	}
	res.Sampled = len(sampled)

	for _, e := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if _, ok := sampled[e.Path]; ok {
			res.verifyPublished(snapshot, e)
		}
		if previous == nil || !sharedInode(previous, snapshot, e.Path) {
			continue
		}
		res.Relinked++
		if source == nil {
			// The digest of a relinked entry was inherited, and only a re-read of
			// the source can prove it. Without a source handle that proof is
			// missing, so the gap is counted instead of glossed over.
			res.SourceUnchecked++
			continue
		}
		res.verifySource(source, e)
	}
	return res, nil
}

// verifyPublished re-hashes one entry inside the snapshot and compares it with
// the recorded digest.
func (r *VerifyResult) verifyPublished(snapshot *os.Root, e Entry) {
	info, err := snapshot.Lstat(e.Path)
	if err != nil {
		r.fail(e.Path, ReasonUnreadable, e.SHA256, "")
		return
	}
	if !info.Mode().IsRegular() {
		// A symlink is called out on its own: it is the shape an attempt to make
		// verification read outside the snapshot takes, and it is a failure even
		// when its target happens to hold the recorded bytes.
		reason := ReasonNotRegular
		if info.Mode()&fs.ModeSymlink != 0 {
			reason = ReasonSymlink
		}
		r.fail(e.Path, reason, e.SHA256, "")
		return
	}
	got, err := hashEntry(snapshot, e.Path)
	if err != nil {
		r.fail(e.Path, ReasonUnreadable, e.SHA256, "")
		return
	}
	if got != e.SHA256 {
		r.fail(e.Path, ReasonVerifyMismatch, e.SHA256, got)
		return
	}
	r.PublishedVerified++
}

// verifySource re-hashes one relinked entry in the source tree and compares it
// with the digest inherited from the previous snapshot.
func (r *VerifyResult) verifySource(source *os.Root, e Entry) {
	info, err := source.Lstat(e.Path)
	if err != nil {
		r.fail(e.Path, ReasonSourceUnreadable, e.SHA256, "")
		return
	}
	if !info.Mode().IsRegular() {
		r.fail(e.Path, ReasonSourceNotRegular, e.SHA256, "")
		return
	}
	got, err := hashEntry(source, e.Path)
	if err != nil {
		r.fail(e.Path, ReasonSourceUnreadable, e.SHA256, "")
		return
	}
	if got != e.SHA256 {
		r.fail(e.Path, ReasonSourceMismatch, e.SHA256, got)
		return
	}
	r.SourceVerified++
}

// hashEntry returns the sha256 of one file, read through root.
//
// root.Open follows symbolic links inside the root, which is why every caller
// checks the entry is a regular file first: the walk this package publishes
// never records a link, so one that appears later is evidence, not a file.
func hashEntry(root *os.Root, rel string) (string, error) {
	f, err := root.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sharedInode reports whether the published file is the very file of the
// previous snapshot, which is what a relink leaves behind.
func sharedInode(previous, snapshot *os.Root, rel string) bool {
	prevInfo, err := previous.Stat(rel)
	if err != nil {
		return false
	}
	current, err := snapshot.Stat(rel)
	if err != nil {
		return false
	}
	return os.SameFile(prevInfo, current)
}

// openPreviousSnapshot returns the newest snapshot older than snapshotDir in the
// same destination, or nil when there is none.
//
// Only directories named like a snapshot and holding a readable manifest are
// considered, for the same reason the writer skips them: an operator's note or a
// half-written tree is not a previous snapshot. A missing predecessor is not an
// error - it means no digest of this snapshot can have been inherited.
func openPreviousSnapshot(snapshotDir string) (*os.Root, error) {
	name := filepath.Base(snapshotDir)
	if _, err := time.Parse(snapshotTimeLayout, name); err != nil {
		// Not a snapshot name, so there is no chronological predecessor to trust.
		return nil, nil
	}
	dest := filepath.Dir(snapshotDir)

	entries, err := os.ReadDir(dest)
	if err != nil {
		return nil, fmt.Errorf("read destination %s: %w", dest, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() >= name {
			continue
		}
		if _, err := time.Parse(snapshotTimeLayout, e.Name()); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	for _, candidate := range names {
		dir := filepath.Join(dest, candidate)
		if _, err := ReadFile(filepath.Join(dir, ManifestName)); err != nil {
			// An unusable index is not a predecessor this build can compare
			// against; the snapshot it belongs to is somebody else's problem.
			continue
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return nil, fmt.Errorf("open previous snapshot %s: %w", dir, err)
		}
		return root, nil
	}
	return nil, nil
}

// sampleEntries selects the entries to verify.
//
// The selection is derived from the manifest digest and the path, never from a
// random source, so two runs over one manifest verify the same files and two
// snapshots of an identical tree verify the same relative paths. The count is
// rounded up, and never below one entry while there is something to check, so a
// small tree is still exercised.
func sampleEntries(manifest *Manifest, all bool, percent int) []Entry {
	n := len(manifest.Files)
	if n == 0 {
		return nil
	}
	if all || percent >= 100 {
		return append([]Entry(nil), manifest.Files...)
	}
	want := (n*percent + 99) / 100
	if want < 1 {
		want = 1
	}
	if want > n {
		want = n
	}

	// The digest is the seed, so the sample is a property of the manifest and not
	// of where the snapshot happens to live or of the order the caller asks in. A
	// NUL cannot appear in a path, so seed and path cannot be forged into each
	// other.
	seed := manifest.ComputeDigest()
	type keyedEntry struct {
		key   string
		entry Entry
	}
	keyed := make([]keyedEntry, n)
	for i, e := range manifest.Files {
		sum := sha256.Sum256([]byte(seed + "\x00" + e.Path))
		keyed[i] = keyedEntry{key: hex.EncodeToString(sum[:]), entry: e}
	}
	sort.Slice(keyed, func(i, j int) bool {
		if keyed[i].key != keyed[j].key {
			return keyed[i].key < keyed[j].key
		}
		return keyed[i].entry.Path < keyed[j].entry.Path
	})

	sampled := make([]Entry, 0, want)
	for _, k := range keyed[:want] {
		sampled = append(sampled, k.entry)
	}
	// Returned in manifest order, so a failure list reads in path order.
	sort.Slice(sampled, func(i, j int) bool { return sampled[i].Path < sampled[j].Path })
	return sampled
}
