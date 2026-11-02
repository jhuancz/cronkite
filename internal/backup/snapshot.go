package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhuancz/cronkite/internal/fsx"
)

// snapshotTimeLayout is the directory name of a snapshot: UTC, fixed width so
// names sort chronologically as plain strings, and free of the separators some
// filesystems refuse.
const snapshotTimeLayout = "20060102T150405Z"

// stagePrefix marks the temporary directory a snapshot is built in. It can
// never be mistaken for a snapshot, because it does not parse as a timestamp.
const stagePrefix = ".stage-"

// Reasons a source entry is left out of a snapshot. They are the same vocabulary
// the cleanup tasks use for their skips.
const (
	// ReasonSymlink marks a symbolic link. It is never followed: cronkite cannot
	// prove what it points at, and copying the target would put data in a
	// snapshot that was never configured as a source.
	ReasonSymlink = "symlink"
	// ReasonNotRegular marks devices, sockets and FIFOs.
	ReasonNotRegular = "not_a_regular_file"
	// ReasonReservedName marks a source entry whose name is the snapshot's own
	// index file.
	ReasonReservedName = "reserved_name"
	// ReasonNotRepresentable marks a source entry whose name cannot be written
	// into a portable manifest: a Unix file name may contain a backslash, which
	// the manifest refuses because on Windows it would restore as a separator.
	ReasonNotRepresentable = "not_representable"
	// ReasonUnreadable marks an entry whose metadata or directory could not be
	// read. It is recorded, not fatal: one unreadable directory must not hide
	// the rest of the tree from the backup.
	ReasonUnreadable = "unreadable"
)

// Skip is one source entry deliberately left out of a snapshot.
type Skip struct {
	// Path is relative to the source root and slash-separated, except for the
	// root itself.
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Written is one file the snapshot holds, with the way the writer produced it.
//
// The counts in Result say how much work a snapshot took; this says what the
// work was, per path, so a caller can report a backup as the snapshot it
// published instead of inferring an entry list from numbers it cannot attribute.
// A relink is not a copy: the kinds are kept apart here because the operator
// pays for the difference in disk and in time.
type Written struct {
	// Path is relative to the snapshot root and slash-separated.
	Path string `json:"path"`
	// Size is the size of the file in bytes.
	Size int64 `json:"size"`
	// Shared reports whether the bytes came from the previous snapshot through
	// a hardlink instead of being written again.
	Shared bool `json:"shared,omitempty"`
}

// Fallback is one file that could not be hardlinked from the previous snapshot
// and had to be copied again.
//
// It exists so a report can tell "this snapshot reuses the previous one" from
// "this snapshot shares no blocks with it": a fallback is a block the
// filesystem made us spend, and it is never silent.
type Fallback struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Result reports what one snapshot produced.
//
// On a failed run the counts describe the abandoned work: no snapshot exists and
// Snapshot is empty, because a half-built tree is never published.
type Result struct {
	// Snapshot is the published snapshot directory.
	Snapshot string `json:"snapshot"`
	// Copied is the number of files whose bytes were written, including files
	// copied because a hardlink failed.
	Copied int `json:"files_copied"`
	// Relinked is the number of files shared with the previous snapshot.
	Relinked int `json:"files_relinked"`
	// RelinkedUnverified is the subset of Relinked that was shared on the
	// strength of size and modification time alone, without re-reading the
	// source. It is zero under the default policy, and it exists so a fast-path
	// run can never be mistaken for a verified one.
	RelinkedUnverified int `json:"files_relinked_unverified,omitempty"`
	// BytesCopied is the number of bytes read from the source and written.
	BytesCopied int64 `json:"bytes_copied"`
	// BytesRelinked is the number of bytes the previous snapshot supplied, which
	// is what incremented backups save.
	BytesRelinked int64 `json:"bytes_deduplicated"`
	// Files are the entries the snapshot holds, in the order the source was
	// walked, each marked as written or shared with the previous snapshot.
	Files []Written `json:"files,omitempty"`
	// Fallbacks are the files that had to be copied after a failed hardlink.
	Fallbacks []Fallback `json:"relink_fallbacks,omitempty"`
	// Skipped are the source entries left out, with the reason.
	Skipped []Skip `json:"skipped,omitempty"`
	// Digest is the aggregate manifest digest of the snapshot.
	Digest string `json:"manifest_digest"`
}

// Writer writes incremental snapshots of one confined source tree.
//
// It is not safe for concurrent use: two snapshots written at once would race
// for the same previous snapshot and the same destination names.
type Writer struct {
	dest   string
	source *fsx.Root
	// read is a second, read-only handle on the source, used to read the bytes
	// of a file. fsx.Root confines walks and removals; the bytes are read
	// through os.Root for the same reason, so a file swapped for a symlink after
	// the walk fails instead of copying something outside the tree.
	read *os.Root
	// link is os.Link in production, and a seam for the tests: the fallback path
	// must be exercised without depending on a second filesystem being mounted.
	link func(oldname, newname string) error
	// mode decides when a file may be shared instead of written again.
	mode RelinkMode
}

// NewWriter prepares to snapshot the roots owned by guard into dest.
//
// The destination is the directory that holds this task's snapshots; the
// timestamp directory and the layout inside it are added here. Exactly one
// source root is accepted, because one snapshot is one complete restorable
// tree, and the destination must sit outside it: a snapshot inside a source
// would be walked into the next snapshot and grow without bound.
func NewWriter(guard *fsx.Guard, dest string) (*Writer, error) {
	roots := guard.Roots()
	if len(roots) != 1 {
		return nil, fmt.Errorf("backup: one snapshot holds one tree, but %d source roots are configured", len(roots))
	}
	source := roots[0]

	abs, err := filepath.Abs(dest)
	if err != nil {
		return nil, fmt.Errorf("resolve destination %q: %w", dest, err)
	}
	// One form of the path everywhere: the containment check, the messages, the
	// report and the directories actually created all use the resolved path, so a
	// report never names a destination the run did not write to. Windows exposed
	// this with 8.3 short names (RUNNER~1), where the same directory had two
	// spellings and only one of them reached the operator.
	dest = resolve(abs)
	if withinFS(source.Dir(), dest) {
		return nil, fmt.Errorf("backup: destination %q is inside source root %q", dest, source.Dir())
	}

	read, err := os.OpenRoot(source.Dir())
	if err != nil {
		return nil, fmt.Errorf("open source root %q for reading: %w", source.Dir(), err)
	}
	return &Writer{dest: dest, source: source, read: read, link: os.Link, mode: RelinkChecksum}, nil
}

// SetRelinkMode selects how strictly a file must be proven unchanged before it
// is shared with the previous snapshot. The default, RelinkChecksum, re-reads
// the source; RelinkSizeMtime trades that guarantee for speed and reports every
// file it shares without re-reading as RelinkedUnverified.
func (w *Writer) SetRelinkMode(mode RelinkMode) error {
	switch mode {
	case RelinkChecksum, RelinkSizeMtime:
		w.mode = mode
		return nil
	default:
		return fmt.Errorf("backup: unknown relink mode %q", mode)
	}
}

// Close releases the source handle.
func (w *Writer) Close() error {
	if w.read == nil {
		return nil
	}
	err := w.read.Close()
	w.read = nil
	return err
}

// dirMode is one snapshot directory whose source permission bits are applied
// once the tree below it has been written.
type dirMode struct {
	path string
	mode fs.FileMode
}

// Write builds one snapshot and publishes it under its timestamp directory.
//
// The tree is written into a temporary directory inside the destination and
// renamed into place only after every file and the manifest are on disk, so an
// interrupted or failed run leaves nothing that looks like a backup.
func (w *Writer) Write(ctx context.Context, now time.Time) (*Result, error) {
	res := &Result{}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	name := now.UTC().Format(snapshotTimeLayout)
	final := filepath.Join(w.dest, name)

	if err := os.MkdirAll(w.dest, 0o755); err != nil {
		return res, fmt.Errorf("create destination %s: %w", w.dest, err)
	}
	// Refuse to replace a snapshot: the name is the timestamp, and a backup that
	// already exists is someone's restore point.
	if _, err := os.Lstat(final); err == nil {
		return res, fmt.Errorf("snapshot %s already exists: refusing to replace a snapshot", final)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("check snapshot %s: %w", final, err)
	}

	prevDir, prev, err := w.previous()
	if err != nil {
		return res, err
	}

	w.sweepStaleStages()

	stage, err := os.MkdirTemp(w.dest, stagePrefix+name+"-")
	if err != nil {
		return res, fmt.Errorf("create working directory in %s: %w", w.dest, err)
	}
	published := false
	defer func() {
		if !published {
			os.RemoveAll(stage)
		}
	}()

	// Resolved once, not per file: the operating system may address the same
	// directory through a symlinked ancestor (macOS /var is /private/var), and the
	// containment check for a relink target has to compare real paths.
	prevResolved := resolveSnapshot(prevDir)

	manifest := NewManifest()
	// Directories whose entry list changed, so each one can be flushed before the
	// snapshot is published: syncing only the stage root would leave nested
	// entries vulnerable to a power loss that the manifest no longer matches.
	touchedDirs := map[string]struct{}{}
	var dirsToSync []string
	// Directories whose source mode is applied last: a directory restored with
	// its real mode may be read-only, and the files inside it could then no
	// longer be written.
	var dirModes []dirMode
	walkErr := w.source.Walk(0, func(abs, rel string, d fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		relSlash := filepath.ToSlash(rel)
		// What a snapshot does with an entry is decided in one place, shared with
		// Plan: a rehearsal that counted a file a run would leave out would be
		// describing a different snapshot.
		f := classify(relSlash, d)
		if f.Skip != nil {
			res.Skipped = append(res.Skipped, *f.Skip)
			if f.SkipTree {
				return fs.SkipDir
			}
			return nil
		}

		dst := filepath.Join(stage, filepath.FromSlash(relSlash))
		if f.Dir != nil {
			// Created even when it will hold nothing: an application that loses its
			// upload or cache directory has not been restored.
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", dst, err)
			}
			collectSyncDirs(touchedDirs, &dirsToSync, stage, dst)
			dirModes = append(dirModes, dirMode{path: dst, mode: f.Perm})
			return manifest.AddDir(*f.Dir)
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
		}
		collectSyncDirs(touchedDirs, &dirsToSync, stage, filepath.Dir(dst))
		entry := *f.File

		// Incrementality is decided from the manifest, not from the source, and
		// by the same helper Plan calls: a file whose record still matches the
		// source is supplied by the previous snapshot, everything else is copied.
		if target, shared, ok := w.relinkSource(prev, prevResolved, relSlash, entry); ok {
			err := w.link(target, dst)
			if err == nil {
				res.Relinked++
				if w.mode != RelinkChecksum {
					res.RelinkedUnverified++
				}
				res.BytesRelinked += shared.Size
				res.Files = append(res.Files, Written{Path: relSlash, Size: shared.Size, Shared: true})
				return manifest.Add(shared)
			}
			// A hardlink needs both snapshots on one filesystem, and a Windows run
			// needs the privilege for it. Copying is the right answer; doing it
			// quietly is not.
			res.Fallbacks = append(res.Fallbacks, Fallback{Path: relSlash, Reason: err.Error()})
		}

		digest, size, err := w.copyFile(dst, relSlash, f.Perm, entry.ModTime)
		if err != nil {
			return fmt.Errorf("copy %s: %w", relSlash, err)
		}
		entry.Size = size
		entry.SHA256 = digest
		res.Copied++
		res.BytesCopied += size
		res.Files = append(res.Files, Written{Path: relSlash, Size: size})
		return manifest.Add(entry)
	}, func(abs string, err error) {
		res.Skipped = append(res.Skipped, Skip{Path: w.relSlash(abs), Reason: ReasonUnreadable})
	})
	if walkErr != nil {
		return res, fmt.Errorf("walk source %s: %w", w.source.Dir(), walkErr)
	}

	// The source mode goes on last, deepest first: everything below each directory
	// is already written, so a read-only source directory no longer blocks it. A
	// platform that cannot express the mode (Windows keeps permission bits as a
	// hint) is not an error, and neither is a filesystem that refuses the change:
	// the tree is still complete, only less faithful.
	for i := len(dirModes) - 1; i >= 0; i-- {
		d := dirModes[i]
		if err := os.Chmod(d.path, d.mode); err != nil && runtime.GOOS != "windows" {
			return res, fmt.Errorf("apply source mode %#o to snapshot directory %s: %w", d.mode, d.path, err)
		}
	}

	if err := manifest.WriteFile(filepath.Join(stage, ManifestName)); err != nil {
		return res, fmt.Errorf("write snapshot manifest: %w", err)
	}
	// Durability, not only atomicity: without these the rename could survive a
	// power loss while the data behind it did not, leaving a directory that looks
	// like a complete snapshot and is not. Deepest first, then the stage root.
	sort.Slice(dirsToSync, func(i, j int) bool {
		return strings.Count(dirsToSync[i], string(filepath.Separator)) > strings.Count(dirsToSync[j], string(filepath.Separator))
	})
	for _, dir := range dirsToSync {
		if err := syncDir(dir); err != nil {
			return res, fmt.Errorf("flush snapshot directory %s: %w", dir, err)
		}
	}
	if err := syncDir(stage); err != nil {
		return res, fmt.Errorf("flush snapshot directory %s: %w", stage, err)
	}
	if err := os.Rename(stage, final); err != nil {
		return res, fmt.Errorf("publish snapshot %s: %w", final, err)
	}
	if err := syncDir(w.dest); err != nil {
		return res, fmt.Errorf("flush destination %s: %w", w.dest, err)
	}
	published = true

	res.Snapshot = final
	res.Digest = manifest.Digest
	return res, nil
}

// Plan is what a snapshot would do, decided without writing anything.
//
// It is the rehearsal of one Write: the same files, the same skips, the same
// relink decision, and the same byte counts, computed from a read-only walk and
// the manifest of the newest readable snapshot.
type Plan struct {
	// Snapshot is the directory a run at the planned time would publish.
	Snapshot string `json:"snapshot"`
	// Previous is the snapshot a run would relink against. Empty when the
	// destination holds no readable snapshot yet.
	Previous string `json:"previous,omitempty"`
	// Copied is the number of files whose bytes a run would write.
	Copied int `json:"files_copied"`
	// Relinked is the number of files a run would share with Previous.
	Relinked int `json:"files_relinked"`
	// BytesCopied is the number of bytes a run would read from the source and
	// write. It is the figure a size ceiling is about.
	BytesCopied int64 `json:"bytes_copied"`
	// BytesRelinked is the number of bytes the previous snapshot would supply,
	// which is what a run would not have to store again.
	BytesRelinked int64 `json:"bytes_deduplicated"`
	// Skipped are the source entries a run would leave out, with the reason.
	Skipped []Skip `json:"skipped,omitempty"`
}

// Files is the number of files a planned snapshot would hold.
func (p *Plan) Files() int { return p.Copied + p.Relinked }

// Plan reports the snapshot a Write at now would publish, and creates nothing.
//
// Every decision comes from the code the writer runs: classify decides what
// belongs in a snapshot, relinkSource decides what the newest readable snapshot
// may supply, and both are called here rather than reimplemented, because a plan
// that drifts from the run it describes is worse than no plan. Under the default
// RelinkChecksum policy that means hashing each candidate for relinking, exactly
// the cost a run pays to prove it may skip the copy.
//
// Two things a plan cannot know, and therefore does not promise: whether the
// filesystem will refuse a hardlink (a fallback copies the file, so the run
// copies more than the plan said), and whether a source file changes between the
// rehearsal and the run. Nothing is created here - no destination directory, no
// staging tree, no snapshot - so a rehearsal of a fresh destination leaves no
// trace at all.
func (w *Writer) Plan(ctx context.Context, now time.Time) (*Plan, error) {
	res := &Plan{Snapshot: filepath.Join(w.dest, now.UTC().Format(snapshotTimeLayout))}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	prevDir, prev, err := w.previous()
	if err != nil {
		return nil, err
	}
	res.Previous = prevDir
	prevResolved := resolveSnapshot(prevDir)

	walkErr := w.source.Walk(0, func(abs, rel string, d fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		relSlash := filepath.ToSlash(rel)
		f := classify(relSlash, d)
		if f.Skip != nil {
			res.Skipped = append(res.Skipped, *f.Skip)
			if f.SkipTree {
				return fs.SkipDir
			}
			return nil
		}
		if f.Dir != nil {
			// Directories carry no bytes and are recreated by the run; they are
			// not part of what an operator plans against.
			return nil
		}

		if _, shared, ok := w.relinkSource(prev, prevResolved, relSlash, *f.File); ok {
			res.Relinked++
			res.BytesRelinked += shared.Size
			return nil
		}
		res.Copied++
		res.BytesCopied += f.File.Size
		return nil
	}, func(abs string, err error) {
		res.Skipped = append(res.Skipped, Skip{Path: w.relSlash(abs), Reason: ReasonUnreadable})
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk source %s: %w", w.source.Dir(), walkErr)
	}
	return res, nil
}

// fate is what a snapshot does with one source entry.
//
// It is the vocabulary Plan and Write share: an entry is either left out (with
// the reason), a directory the snapshot recreates, or a file the snapshot holds.
type fate struct {
	// Skip is set when the entry is left out of the snapshot.
	Skip *Skip
	// SkipTree reports that everything below the entry is left out as well.
	SkipTree bool
	// Dir is the directory record a snapshot holds for the entry.
	Dir *Dir
	// File is the file record a snapshot would hold, with size and mtime but no
	// digest: the digest is only known once the bytes are read.
	File *Entry
	// Perm are the source permission bits, which a snapshot reproduces.
	Perm fs.FileMode
}

// classify decides what a snapshot does with one source entry.
//
// The rules live in one function because both the writer and the planner ask the
// same question, and because each rule is a way for content to enter or leave a
// snapshot that must not differ between them: the manifest's own name is refused
// at the root (a file or a directory, in any case, because macOS and Windows
// resolve names case-insensitively), a name the manifest cannot express costs
// that file and never the snapshot, symlinks and special files are left out, and
// metadata that cannot be read is reported rather than silently dropped.
func classify(relSlash string, d fs.DirEntry) fate {
	if path.Dir(relSlash) == "." && strings.EqualFold(relSlash, ManifestName) {
		// The whole subtree belongs to the collision, not just the entry.
		return fate{Skip: &Skip{Path: relSlash, Reason: ReasonReservedName}, SkipTree: d.IsDir()}
	}

	if d.IsDir() {
		if !representable(relSlash) {
			// Nothing below it can be represented either: every path in the
			// subtree carries the same component.
			return fate{Skip: &Skip{Path: relSlash, Reason: ReasonNotRepresentable}, SkipTree: true}
		}
		info, err := d.Info()
		if err != nil {
			return fate{Skip: &Skip{Path: relSlash, Reason: ReasonUnreadable}, SkipTree: true}
		}
		perm := info.Mode().Perm()
		return fate{
			Dir:  &Dir{Path: relSlash, ModTime: info.ModTime().UTC(), Mode: uint32(perm)},
			Perm: perm,
		}
	}

	switch {
	case d.Type()&fs.ModeSymlink != 0:
		return fate{Skip: &Skip{Path: relSlash, Reason: ReasonSymlink}}
	case !d.Type().IsRegular():
		return fate{Skip: &Skip{Path: relSlash, Reason: ReasonNotRegular}}
	case !representable(relSlash):
		// The manifest refuses some names on purpose, so the walk has to refuse
		// them first: one legacy file name must cost that file, never every
		// snapshot this task will ever write.
		return fate{Skip: &Skip{Path: relSlash, Reason: ReasonNotRepresentable}}
	}

	info, err := d.Info()
	if err != nil {
		return fate{Skip: &Skip{Path: relSlash, Reason: ReasonUnreadable}}
	}
	return fate{
		File: &Entry{Path: relSlash, Size: info.Size(), ModTime: info.ModTime().UTC()},
		Perm: info.Mode().Perm(),
	}
}

// relinkSource decides whether the previous snapshot supplies this file, and
// returns the link target together with the record it supplies.
//
// It is the single decision behind incrementality, so Plan and Write cannot
// disagree: a file the previous snapshot may supply is counted as a relink by
// both, and one it may not is copied by both.
func (w *Writer) relinkSource(prev *Manifest, prevResolved, rel string, entry Entry) (string, Entry, bool) {
	shared, ok := prev.Lookup(rel)
	if !ok || !w.relinkable(rel, shared, entry) {
		return "", Entry{}, false
	}
	target, ok := relinkTarget(prevResolved, rel, shared)
	if !ok {
		return "", Entry{}, false
	}
	return target, shared, true
}

// resolveSnapshot returns the real path of a snapshot directory, so the
// containment check for a relink target compares like with like when an ancestor
// is a symlink. An unknown directory resolves to itself.
func resolveSnapshot(dir string) string {
	if dir == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

// collectSyncDirs records dir and its ancestors below stage, so every directory
// whose entry list changed is flushed before the snapshot is published.
func collectSyncDirs(seen map[string]struct{}, list *[]string, stage, dir string) {
	for ; dir != stage && within(stage, dir); dir = filepath.Dir(dir) {
		if _, ok := seen[dir]; ok {
			// The ancestors of a registered directory were registered with it.
			return
		}
		seen[dir] = struct{}{}
		*list = append(*list, dir)
	}
}

// stageMaxAge is how long an abandoned stage directory may survive. Only a
// hard kill leaves one behind, and a live stage of a correctly locked run is
// seconds old.
const stageMaxAge = time.Hour

// sweepStaleStages removes stage directories left by a killed run. It is
// best-effort and age-gated, so a stage that is being written right now is never
// taken away; the run lock is what makes that judgement safe in the first place.
// Without the sweep a killed relink run keeps pinning previous-snapshot inodes
// that no retention pass can free.
func (w *Writer) sweepStaleStages() {
	entries, err := os.ReadDir(w.dest)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), stagePrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < stageMaxAge {
			continue
		}
		os.RemoveAll(filepath.Join(w.dest, e.Name()))
	}
}

// previous returns the newest snapshot in the destination and its manifest.
//
// Directories whose name is not a snapshot timestamp, and snapshot-shaped
// directories without a manifest, are passed over: an operator's note or a
// tree left by an older build must not cost the next run its links. A manifest
// this build cannot read is an error instead, because silently copying
// everything would hide that the previous snapshot is unusable.
func (w *Writer) previous() (string, *Manifest, error) {
	entries, err := os.ReadDir(w.dest)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("read destination %s: %w", w.dest, err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := time.Parse(snapshotTimeLayout, e.Name()); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	for _, name := range names {
		dir := filepath.Join(w.dest, name)
		m, err := ReadFile(filepath.Join(dir, ManifestName))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", nil, fmt.Errorf("previous snapshot %s: %w", dir, err)
		}
		return dir, m, nil
	}
	return "", nil, nil
}

// copyFile copies one source file into the snapshot, gives it back the source
// modification time and returns the digest and the number of bytes written.
//
// The timestamp matters as much as the bytes: a restored tree whose files all
// look freshly written breaks anything that reasons about age, and the manifest
// records the source mtime, so the copy has to agree with it.
func (w *Writer) copyFile(dst, relSlash string, perm fs.FileMode, modTime time.Time) (string, int64, error) {
	in, err := w.read.Open(filepath.FromSlash(relSlash))
	if err != nil {
		return "", 0, err
	}
	defer in.Close()

	if perm == 0 {
		perm = 0o644
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return "", 0, err
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		out.Close()
		return "", n, err
	}
	if modTime.IsZero() {
		modTime = time.Now()
	}
	if err := os.Chtimes(dst, modTime, modTime); err != nil {
		out.Close()
		return "", n, err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return "", n, err
	}
	if err := out.Close(); err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// RelinkMode decides when a source file may be shared with the previous
// snapshot instead of being written again.
type RelinkMode string

const (
	// RelinkChecksum is the default: the source is re-read and its digest
	// compared with the previous snapshot before the file is shared. It costs a
	// read of every unchanged file, and it cannot be fooled by content that
	// changed while size and modification time stayed the same.
	RelinkChecksum RelinkMode = "checksum"
	// RelinkSizeMtime is the fast policy link-dest snapshots use: equal size and
	// modification time are taken to mean equal bytes. Every file it shares is
	// counted in RelinkedUnverified, so the choice is visible in the report and
	// no run can claim a guarantee it did not check.
	RelinkSizeMtime RelinkMode = "size_mtime"
)

// foldCase reports whether path comparisons on this platform must ignore case.
const foldCase = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// relinkable reports whether the previous snapshot may supply this file.
func (w *Writer) relinkable(rel string, shared, entry Entry) bool {
	if shared.Size != entry.Size || !shared.ModTime.Equal(entry.ModTime) {
		return false
	}
	if w.mode != RelinkChecksum {
		return true
	}
	digest, err := w.digestFile(rel)
	return err == nil && digest == shared.SHA256
}

// digestFile hashes one source file through the confined handle.
func (w *Writer) digestFile(rel string) (string, error) {
	f, err := w.read.Open(filepath.FromSlash(rel))
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

// representable reports whether a source name can be written into a portable
// manifest. Whatever the manifest would refuse has to be skipped here, because
// a name it refuses would otherwise abort the whole snapshot.
func representable(rel string) bool {
	if !utf8.ValidString(rel) || strings.ContainsRune(rel, '\\') {
		return false
	}
	return !(path.Dir(rel) == "." && rel == ":")
}

// relinkTarget returns the previous snapshot file that may supply this entry, or
// false when the previous snapshot cannot be trusted to hold it.
//
// Three things are checked, and each one closes a way for content to enter a
// published snapshot that was never in the source:
//   - the entry is a regular file (a planted symlink or a directory is refused);
//   - it resolves inside the previous snapshot, which a symlinked intermediate
//     directory would otherwise break, since Lstat does not inspect ancestors;
//   - its size and mtime still match the record whose digest we are about to
//     reuse, so a file edited inside a published snapshot is copied instead.
func relinkTarget(prevResolvedDir, rel string, shared Entry) (string, bool) {
	target := filepath.Join(prevResolvedDir, filepath.FromSlash(rel))
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil || !within(prevResolvedDir, resolved) {
		return "", false
	}
	// Compared at second resolution: filesystems store timestamps with varying
	// precision, and a nanosecond that was truncated on write must not be read as
	// tampering. A rewrite still moves the seconds, so the guard keeps working.
	if info.Size() != shared.Size ||
		!info.ModTime().Truncate(time.Second).Equal(shared.ModTime.Truncate(time.Second)) {
		return "", false
	}
	return target, true
}

// syncDir flushes a directory's entries to stable storage.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		// Windows has no directory fsync: FlushFileBuffers needs a writable
		// handle and fails with access denied on a directory opened read-only, so
		// insisting on it would refuse every snapshot on that platform. NTFS
		// journals the metadata instead.
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// withinFS reports whether p sits inside root, comparing case-insensitively on
// the platforms whose filesystems usually are. On such a volume "photos" and
// "Photos" are one directory, and a byte-wise comparison would cheerfully put
// snapshots inside the source.
func withinFS(root, p string) bool {
	if foldCase {
		return within(strings.ToLower(root), strings.ToLower(p))
	}
	return within(root, p)
}

// relSlash maps an absolute path inside the source to a snapshot-relative,
// slash-separated one.
func (w *Writer) relSlash(abs string) string {
	if rel, ok := w.source.Rel(abs); ok {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(abs)
}

// within reports whether p is root or sits below it.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." ||
		(rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolve follows symlinked ancestors of p, as far as they exist. Operators
// address the same directory through a link all the time (macOS /var is
// /private/var, containers do the same with /tmp), and the containment check
// has to compare real paths to mean anything. A destination that does not exist
// yet is resolved through its deepest existing ancestor.
func resolve(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}

	tail := []string{}
	cur := filepath.Clean(p)
	for {
		parent, base := filepath.Split(cur)
		if base == "" {
			return p
		}
		tail = append([]string{base}, tail...)
		cur = filepath.Clean(parent)
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{resolved}, tail...)...)
		}
	}
}
