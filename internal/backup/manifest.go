// Package backup writes incremental, complete snapshots of a confined tree.
//
// A backup is only worth its disk space if it can be restored, so this package
// does not invent an archive format: one snapshot is a plain directory that
// holds the whole tree, and unchanged files are hardlinked from the previous
// snapshot so the filesystem does the deduplication. Every snapshot is
// therefore complete on its own, restorable with cp, and cheap to compare with
// the one before it.
//
// Two invariants shape the code:
//   - Reads are confined. The source is walked through an fsx.Guard, and bytes
//     are read through an os.Root, so a symlink planted in the tree cannot make
//     a backup copy /etc/passwd into a snapshot.
//   - Nothing is published half-done. A snapshot is built in a temporary
//     directory inside the destination, flushed to stable storage and renamed
//     into place only after its manifest is written. A crashed or failed run
//     leaves nothing an operator could mistake for a backup.
//   - Sharing is proven, not assumed. By default a file is compared with the
//     previous snapshot by content before it is hardlinked; the cheaper
//     size+mtime policy exists, but every file it shares is reported as
//     unverified, so no run can claim more than it checked.
//
// What lives in a snapshot is described by its manifest, which is the contract
// this package publishes. See Manifest for the on-disk format.
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ManifestSchema is the manifest format version. Bump it when a field is
// removed or its meaning changes, never for additive changes.
//
// It stays 1 now that "dirs" exists: the field is additive, a manifest written
// without it reads as "no directory records" instead of failing, and no released
// build has ever read this format, so there is no consumer to migrate.
const ManifestSchema = 1

// ManifestName is the file name of the manifest inside a snapshot directory.
const ManifestName = "manifest.json"

// ErrUnknownSchema marks a manifest written by a format this build cannot read.
// A snapshot is restored, not migrated: reading it with the wrong rules would
// quietly produce a wrong answer, so it is refused instead.
var ErrUnknownSchema = errors.New("unsupported snapshot manifest schema")

// Entry is one file recorded in a snapshot.
//
// Path is always relative to the snapshot root and always slash-separated, on
// every platform, so a manifest written on Windows reads the same on Linux.
// ModTime is stored in UTC.
type Entry struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	SHA256  string    `json:"sha256"`
}

// Dir is one directory recorded in a snapshot.
//
// A snapshot that only recorded files would restore an application without its
// upload or cache directory, so the directory skeleton is part of the index even
// when a directory holds nothing. Path follows the same rules as Entry.Path.
//
// Mode holds the source permission bits (the 0o777 mask): they are advisory on
// Windows, where the platform cannot carry more than a read-only hint, and a
// restore applies them where the platform allows it. ModTime is stored in UTC.
type Dir struct {
	Path    string    `json:"path"`
	ModTime time.Time `json:"mtime"`
	Mode    uint32    `json:"mode"`
}

// Manifest is the index of one snapshot.
//
// Its JSON form is the public surface a future verify command and a restore
// check depend on, so the field names are deliberate:
//
//	{
//	  "schema": 1,                    // manifest format version
//	  "digest": "<64 hex chars>",     // aggregate digest over "files" and "dirs"
//	  "files": [
//	    {
//	      "path": "logs/app.log",     // relative to the snapshot root, slash-separated
//	      "size": 1234,               // bytes
//	      "mtime": "2026-03-01T12:00:00.123456789Z",  // UTC, RFC 3339 with nanoseconds
//	      "sha256": "<64 hex chars>"  // digest of the file content
//	    }
//	  ],
//	  "dirs": [
//	    {
//	      "path": "uploads",          // same path rules as a file record
//	      "mtime": "2026-03-01T12:00:00.123456789Z",
//	      "mode": 493                 // source permission bits, 0o755 here
//	    }
//	  ]
//	}
//
// "files" and "dirs" are each sorted by "path". "digest" covers the records and
// not itself, so the digest of a snapshot can be recomputed from the file and
// compared without trusting the value on disk.
type Manifest struct {
	Schema int     `json:"schema"`
	Digest string  `json:"digest"`
	Files  []Entry `json:"files"`
	Dirs   []Dir   `json:"dirs"`
}

// NewManifest returns an empty manifest carrying the current schema.
func NewManifest() *Manifest {
	return &Manifest{Schema: ManifestSchema, Files: []Entry{}, Dirs: []Dir{}}
}

// Add records one file. Entries are kept sorted by path, and a repeated path or
// a record that could not be restored faithfully is refused rather than stored.
func (m *Manifest) Add(e Entry) error {
	if err := validateEntry(e); err != nil {
		return err
	}
	i := sort.Search(len(m.Files), func(i int) bool { return m.Files[i].Path >= e.Path })
	if i < len(m.Files) && m.Files[i].Path == e.Path {
		return fmt.Errorf("manifest: duplicate entry %q", e.Path)
	}
	m.Files = append(m.Files, Entry{})
	copy(m.Files[i+1:], m.Files[i:])
	m.Files[i] = e
	return nil
}

// AddDir records one directory, keeping the same ordering and duplicate rules as
// Add. A directory is recorded even when it holds no file, because a restore has
// to reproduce the tree and not only its files.
func (m *Manifest) AddDir(d Dir) error {
	if err := validateDir(d); err != nil {
		return err
	}
	i := sort.Search(len(m.Dirs), func(i int) bool { return m.Dirs[i].Path >= d.Path })
	if i < len(m.Dirs) && m.Dirs[i].Path == d.Path {
		return fmt.Errorf("manifest: duplicate entry %q", d.Path)
	}
	m.Dirs = append(m.Dirs, Dir{})
	copy(m.Dirs[i+1:], m.Dirs[i:])
	m.Dirs[i] = d
	return nil
}

// Lookup returns the record for a snapshot-relative, slash-separated path.
//
// A nil manifest misses, so callers fitting a first snapshot and a later one
// into one code path do not need a special case.
func (m *Manifest) Lookup(rel string) (Entry, bool) {
	if m == nil {
		return Entry{}, false
	}
	i := sort.Search(len(m.Files), func(i int) bool { return m.Files[i].Path >= rel })
	if i < len(m.Files) && m.Files[i].Path == rel {
		return m.Files[i], true
	}
	return Entry{}, false
}

// ComputeDigest returns the aggregate digest of the recorded files and
// directories.
//
// Each file record contributes its path, size, mtime in UTC nanoseconds and
// content digest; each directory record contributes its path, mtime and mode, so
// two snapshots that differ only in an empty directory do not share a digest.
// The result depends on the tree and not on the order the records were added.
// Two snapshots of an unchanged tree therefore share a digest, and comparing them
// costs one string comparison instead of a full re-hash.
func (m *Manifest) ComputeDigest() string {
	if m == nil {
		return ""
	}
	files := make([]Entry, len(m.Files))
	copy(files, m.Files)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	dirs := make([]Dir, len(m.Dirs))
	copy(dirs, m.Dirs)
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Path < dirs[j].Path })

	h := sha256.New()
	for _, e := range files {
		// A NUL cannot appear in a path, so the fields cannot be forged into
		// each other.
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%s\n", e.Path, e.Size, e.ModTime.UTC().UnixNano(), hex.EncodeToString(mustDigest(e.SHA256)))
	}
	for _, d := range dirs {
		// A directory line starts with a NUL, which no path may contain, so a
		// file record can never be read as a directory record. A manifest without
		// directory records hashes exactly the bytes it hashed before this field
		// existed, so an older manifest still verifies against its own records.
		//
		// The mode is part of the tree's identity because a restore reproduces it.
		// The directory mtime is not: cronkite never applies it back (writing the
		// children moves it anyway), so folding it into the digest made two
		// snapshots of an unchanged tree disagree for a value the tool does not
		// deliver - which is what Windows exposed.
		fmt.Fprintf(h, "\x00dir\x00%s\x00%o\n", d.Path, d.Mode)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Seal records the current aggregate digest on the manifest.
func (m *Manifest) Seal() { m.Digest = m.ComputeDigest() }

// WriteFile writes the manifest to path atomically: the records are canonicalized
// (sorted, validated), the digest is recomputed, and the file appears in one
// rename. A reader never sees a half-written manifest, which matters because a
// truncated manifest would make a complete snapshot look empty.
func (m *Manifest) WriteFile(path string) error {
	files, dirs, err := m.canonical()
	if err != nil {
		return fmt.Errorf("manifest %s: %w", path, err)
	}
	out := &Manifest{Schema: ManifestSchema, Files: files, Dirs: dirs}
	out.Seal()

	tmp, err := os.CreateTemp(filepath.Dir(path), ".manifest-*.json")
	if err != nil {
		return fmt.Errorf("create manifest next to %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		tmp.Close()
		return fmt.Errorf("write manifest %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("flush manifest %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write manifest %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("publish manifest %s: %w", path, err)
	}
	m.Digest = out.Digest
	return nil
}

// ReadFile decodes a manifest written by cronkite.
//
// An unknown schema is an error and never a silent downgrade: the caller must
// stop, not guess how to interpret an index it does not understand.
func ReadFile(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode manifest %s: %w", path, err)
	}
	if m.Schema != ManifestSchema {
		return nil, fmt.Errorf("%w: %s has schema %d, this build reads schema %d", ErrUnknownSchema, path, m.Schema, ManifestSchema)
	}
	files, dirs, err := m.canonical()
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	m.Files = files
	m.Dirs = dirs
	// A manifest carries its own aggregate digest, so it can be checked instead
	// of trusted. A digest that does not match its records means the file was
	// truncated, edited, or written by something that was not this build.
	if m.Digest != "" {
		if want := m.ComputeDigest(); m.Digest != want {
			return nil, fmt.Errorf("manifest %s: digest %s does not match its records (%s): the file is corrupt or was edited", path, m.Digest, want)
		}
	}
	return &m, nil
}

// canonical validates the records and returns them sorted by path with the
// schema checked, so both writing and reading agree on one shape.
func (m *Manifest) canonical() ([]Entry, []Dir, error) {
	if m.Schema != ManifestSchema {
		return nil, nil, fmt.Errorf("%w: schema is %d, this build writes %d", ErrUnknownSchema, m.Schema, ManifestSchema)
	}
	files := make([]Entry, len(m.Files))
	copy(files, m.Files)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	for i, e := range files {
		if err := validateEntry(e); err != nil {
			return nil, nil, err
		}
		if i > 0 && files[i-1].Path == e.Path {
			return nil, nil, fmt.Errorf("manifest: duplicate entry %q", e.Path)
		}
		// One spelling of a digest: the aggregate digest lowercases internally,
		// so an upper-case record would make two equal trees compare unequal.
		files[i].SHA256 = strings.ToLower(e.SHA256)
	}

	dirs := make([]Dir, len(m.Dirs))
	copy(dirs, m.Dirs)
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Path < dirs[j].Path })
	for i, d := range dirs {
		if err := validateDir(d); err != nil {
			return nil, nil, err
		}
		if i > 0 && dirs[i-1].Path == d.Path {
			return nil, nil, fmt.Errorf("manifest: duplicate entry %q", d.Path)
		}
	}

	// A path cannot be a file and a directory at once, and a manifest that said
	// so would describe two different trees with one digest.
	for i, j := 0, 0; i < len(files) && j < len(dirs); {
		switch {
		case files[i].Path < dirs[j].Path:
			i++
		case dirs[j].Path < files[i].Path:
			j++
		default:
			return nil, nil, fmt.Errorf("manifest: %q is recorded as both a file and a directory", files[i].Path)
		}
	}
	return files, dirs, nil
}

// validateEntry enforces what the manifest promises: a relative,
// slash-separated, clean path and a record complete enough to restore from.
func validateEntry(e Entry) error {
	if err := validatePath(e.Path); err != nil {
		return err
	}
	switch {
	case e.Size < 0:
		return fmt.Errorf("manifest entry %q: negative size %d", e.Path, e.Size)
	case e.ModTime.IsZero():
		return fmt.Errorf("manifest entry %q: missing modification time", e.Path)
	}
	if _, err := hex.DecodeString(e.SHA256); err != nil || len(e.SHA256) != sha256.Size*2 {
		return fmt.Errorf("manifest entry %q: sha256 must be %d hex characters", e.Path, sha256.Size*2)
	}
	return nil
}

// validateDir enforces the same path rules as a file record, plus a mode a
// restore can actually apply.
func validateDir(d Dir) error {
	if err := validatePath(d.Path); err != nil {
		return err
	}
	switch {
	case d.ModTime.IsZero():
		return fmt.Errorf("manifest entry %q: missing modification time", d.Path)
	case d.Mode&^uint32(fs.ModePerm) != 0:
		return fmt.Errorf("manifest entry %q: mode %#o has bits outside the permission mask %#o", d.Path, d.Mode, fs.ModePerm)
	}
	return nil
}

// validatePath is the one set of path rules every record obeys, whichever kind
// of entry it describes.
func validatePath(p string) error {
	switch {
	case p == "":
		return errors.New("manifest entry: empty path")
	case containsBackslash(p):
		return fmt.Errorf("manifest entry %q: paths are slash-separated on every platform", p)
	case !utf8.ValidString(p):
		// JSON cannot carry invalid UTF-8: decoding would coerce the bytes to
		// the replacement character and two distinct names would collapse into
		// one, producing a manifest this build can no longer read.
		return fmt.Errorf("manifest entry %q: path is not valid UTF-8", p)
	case p == ":":
		return fmt.Errorf("manifest entry %q: a bare %q is not a usable path", p, ":")
	case path.IsAbs(p), filepath.IsAbs(p):
		return fmt.Errorf("manifest entry %q: paths are relative to the snapshot root", p)
	case p == "." || p == ".." || strings.HasPrefix(p, "../") || path.Clean(p) != p:
		return fmt.Errorf("manifest entry %q: paths must be clean and must not climb out of the snapshot", p)
	}
	return nil
}

func containsBackslash(p string) bool {
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' {
			return true
		}
	}
	return false
}

func mustDigest(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		return []byte(s)
	}
	return b
}
