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
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	// ReasonReservedName marks a source file named manifest.json at the root of
	// the tree: that name is the snapshot's index.
	ReasonReservedName = "reserved_name"
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
	// BytesCopied is the number of bytes read from the source and written.
	BytesCopied int64 `json:"bytes_copied"`
	// BytesRelinked is the number of bytes the previous snapshot supplied, which
	// is what incremented backups save.
	BytesRelinked int64 `json:"bytes_deduplicated"`
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
	if within(source.Dir(), resolve(abs)) {
		return nil, fmt.Errorf("backup: destination %q is inside source root %q", abs, source.Dir())
	}

	read, err := os.OpenRoot(source.Dir())
	if err != nil {
		return nil, fmt.Errorf("open source root %q for reading: %w", source.Dir(), err)
	}
	return &Writer{dest: abs, source: source, read: read, link: os.Link}, nil
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

	manifest := NewManifest()
	walkErr := w.source.Walk(0, func(abs, rel string, d fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		relSlash := filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			res.Skipped = append(res.Skipped, Skip{Path: relSlash, Reason: ReasonSymlink})
			return nil
		case !d.Type().IsRegular():
			res.Skipped = append(res.Skipped, Skip{Path: relSlash, Reason: ReasonNotRegular})
			return nil
		case relSlash == ManifestName:
			// The index of the snapshot lives at the root of the snapshot, so a
			// source file with that name would be overwritten by it.
			res.Skipped = append(res.Skipped, Skip{Path: relSlash, Reason: ReasonReservedName})
			return nil
		}

		info, err := d.Info()
		if err != nil {
			res.Skipped = append(res.Skipped, Skip{Path: relSlash, Reason: ReasonUnreadable})
			return nil
		}

		dst := filepath.Join(stage, filepath.FromSlash(relSlash))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
		}
		entry := Entry{Path: relSlash, Size: info.Size(), ModTime: info.ModTime().UTC()}

		// Incrementality is decided from the manifest, not from the source: a
		// file whose size and mtime match the previous record, and which is
		// still there, is the same bytes with the same digest.
		if shared, ok := prev.Lookup(relSlash); ok && shared.Size == entry.Size && shared.ModTime.Equal(entry.ModTime) {
			target := filepath.Join(prevDir, filepath.FromSlash(relSlash))
			if _, err := os.Lstat(target); err == nil {
				err := w.link(target, dst)
				if err == nil {
					res.Relinked++
					res.BytesRelinked += shared.Size
					return manifest.Add(shared)
				}
				// A hardlink needs both snapshots on one filesystem, and a Windows
				// run needs the privilege for it. Copying is the right answer;
				// doing it quietly is not.
				res.Fallbacks = append(res.Fallbacks, Fallback{Path: relSlash, Reason: err.Error()})
			}
		}

		digest, size, err := w.copyFile(dst, relSlash, info.Mode().Perm())
		if err != nil {
			return fmt.Errorf("copy %s: %w", relSlash, err)
		}
		entry.Size = size
		entry.SHA256 = digest
		res.Copied++
		res.BytesCopied += size
		return manifest.Add(entry)
	}, func(abs string, err error) {
		res.Skipped = append(res.Skipped, Skip{Path: w.relSlash(abs), Reason: ReasonUnreadable})
	})
	if walkErr != nil {
		return res, fmt.Errorf("walk source %s: %w", w.source.Dir(), walkErr)
	}

	if err := manifest.WriteFile(filepath.Join(stage, ManifestName)); err != nil {
		return res, fmt.Errorf("write snapshot manifest: %w", err)
	}
	if err := os.Rename(stage, final); err != nil {
		return res, fmt.Errorf("publish snapshot %s: %w", final, err)
	}
	published = true

	res.Snapshot = final
	res.Digest = manifest.Digest
	return res, nil
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

// copyFile copies one source file into the snapshot and returns the digest and
// the number of bytes written.
func (w *Writer) copyFile(dst, relSlash string, perm fs.FileMode) (string, int64, error) {
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
	if err := out.Close(); err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
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
