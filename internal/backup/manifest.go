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
//     directory inside the destination and renamed into place only after its
//     manifest is written. A crashed or failed run leaves nothing an operator
//     could mistake for a backup.
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
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ManifestSchema is the manifest format version. Bump it when a field is
// removed or its meaning changes, never for additive changes.
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

// Manifest is the index of one snapshot.
//
// Its JSON form is the public surface a future verify command and a restore
// check depend on, so the field names are deliberate:
//
//	{
//	  "schema": 1,                    // manifest format version
//	  "digest": "<64 hex chars>",     // aggregate digest over "files"
//	  "files": [
//	    {
//	      "path": "logs/app.log",     // relative to the snapshot root, slash-separated
//	      "size": 1234,               // bytes
//	      "mtime": "2026-03-01T12:00:00.123456789Z",  // UTC, RFC 3339 with nanoseconds
//	      "sha256": "<64 hex chars>"  // digest of the file content
//	    }
//	  ]
//	}
//
// "files" is sorted by "path". "digest" covers the records and not itself, so
// the digest of a snapshot can be recomputed from the file and compared without
// trusting the value on disk.
type Manifest struct {
	Schema int     `json:"schema"`
	Digest string  `json:"digest"`
	Files  []Entry `json:"files"`
}

// NewManifest returns an empty manifest carrying the current schema.
func NewManifest() *Manifest {
	return &Manifest{Schema: ManifestSchema, Files: []Entry{}}
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

// ComputeDigest returns the aggregate digest of the recorded files.
//
// Each record contributes its path, size, mtime in UTC nanoseconds and content
// digest, so the result depends on the tree and not on the order the records
// were added. Two snapshots of an unchanged tree therefore share a digest, and
// comparing them costs one string comparison instead of a full re-hash.
func (m *Manifest) ComputeDigest() string {
	if m == nil {
		return ""
	}
	files := make([]Entry, len(m.Files))
	copy(files, m.Files)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	h := sha256.New()
	for _, e := range files {
		// A NUL cannot appear in a path, so the fields cannot be forged into
		// each other.
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%s\n", e.Path, e.Size, e.ModTime.UTC().UnixNano(), hex.EncodeToString(mustDigest(e.SHA256)))
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
	files, err := m.canonical()
	if err != nil {
		return fmt.Errorf("manifest %s: %w", path, err)
	}
	out := &Manifest{Schema: ManifestSchema, Files: files}
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
	files, err := m.canonical()
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	m.Files = files
	return &m, nil
}

// canonical validates the records and returns them sorted by path with the
// schema checked, so both writing and reading agree on one shape.
func (m *Manifest) canonical() ([]Entry, error) {
	if m.Schema != ManifestSchema {
		return nil, fmt.Errorf("%w: schema is %d, this build writes %d", ErrUnknownSchema, m.Schema, ManifestSchema)
	}
	files := make([]Entry, len(m.Files))
	copy(files, m.Files)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	for i, e := range files {
		if err := validateEntry(e); err != nil {
			return nil, err
		}
		if i > 0 && files[i-1].Path == e.Path {
			return nil, fmt.Errorf("manifest: duplicate entry %q", e.Path)
		}
	}
	return files, nil
}

// validateEntry enforces what the manifest promises: a relative,
// slash-separated, clean path and a record complete enough to restore from.
func validateEntry(e Entry) error {
	switch {
	case e.Path == "":
		return errors.New("manifest entry: empty path")
	case containsBackslash(e.Path):
		return fmt.Errorf("manifest entry %q: paths are slash-separated on every platform", e.Path)
	case path.IsAbs(e.Path), filepath.IsAbs(e.Path):
		return fmt.Errorf("manifest entry %q: paths are relative to the snapshot root", e.Path)
	case e.Path == "." || e.Path == ".." || strings.HasPrefix(e.Path, "../") || path.Clean(e.Path) != e.Path:
		return fmt.Errorf("manifest entry %q: paths must be clean and must not climb out of the snapshot", e.Path)
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
