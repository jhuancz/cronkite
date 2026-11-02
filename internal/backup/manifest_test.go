package backup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var entryTime = time.Date(2026, 3, 1, 12, 0, 0, 123456789, time.UTC)

func testEntry(path string, size int64) Entry {
	return Entry{
		Path:    path,
		Size:    size,
		ModTime: entryTime,
		SHA256:  strings.Repeat("ab", 32),
	}
}

func newTestManifest(t *testing.T, paths ...string) *Manifest {
	t.Helper()
	m := NewManifest()
	for _, p := range paths {
		if err := m.Add(testEntry(p, 1)); err != nil {
			t.Fatalf("Add(%q): %v", p, err)
		}
	}
	return m
}

func TestManifestAddKeepsPathOrder(t *testing.T) {
	m := newTestManifest(t, "b.txt", "a.txt", "c/d.txt")

	want := []string{"a.txt", "b.txt", "c/d.txt"}
	if len(m.Files) != len(want) {
		t.Fatalf("Files = %v, want %v", m.Files, want)
	}
	for i, p := range want {
		if m.Files[i].Path != p {
			t.Fatalf("Files[%d] = %q, want %q (entries must stay in path order)", i, m.Files[i].Path, p)
		}
	}
}

func TestManifestAddRejectsDuplicatePath(t *testing.T) {
	m := newTestManifest(t, "a.txt")
	err := m.Add(testEntry("a.txt", 2))
	if err == nil {
		t.Fatal("expected a duplicate path to be refused")
	}
	if len(m.Files) != 1 || m.Files[0].Size != 1 {
		t.Fatalf("the refused entry must not be recorded: %v", m.Files)
	}
}

func TestManifestLookup(t *testing.T) {
	m := newTestManifest(t, "a.txt", "b/c.log")

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "present", path: "a.txt", want: true},
		{name: "nested", path: "b/c.log", want: true},
		{name: "absent", path: "b/d.log", want: false},
		{name: "absolute", path: "/a.txt", want: false},
		{name: "empty", path: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := m.Lookup(tt.path); ok != tt.want {
				t.Fatalf("Lookup(%q) = %v, want %v", tt.path, ok, tt.want)
			}
		})
	}

	var nilManifest *Manifest
	if _, ok := nilManifest.Lookup("a.txt"); ok {
		t.Fatal("Lookup on a nil manifest must miss, not panic")
	}
}

func TestManifestRejectsInvalidPath(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "absolute", path: "/etc/passwd"},
		{name: "parent traversal", path: "../escape.txt"},
		{name: "nested traversal", path: "a/../b.txt"},
		{name: "bare parent", path: ".."},
		{name: "dot", path: "."},
		{name: "dot segment", path: "./a.txt"},
		{name: "unclean separators", path: "a//b.txt"},
		{name: "trailing separator", path: "a/"},
		{name: "backslash", path: `a\b.txt`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManifest()
			if err := m.Add(Entry{Path: tt.path, Size: 1, ModTime: entryTime, SHA256: strings.Repeat("ab", 32)}); err == nil {
				t.Fatalf("expected %q to be refused", tt.path)
			}
			if len(m.Files) != 0 {
				t.Fatalf("the refused entry must not be recorded: %v", m.Files)
			}
		})
	}
}

func TestManifestRejectsInvalidRecord(t *testing.T) {
	tests := []struct {
		name  string
		entry Entry
	}{
		{
			name:  "negative size",
			entry: Entry{Path: "a.txt", Size: -1, ModTime: entryTime, SHA256: strings.Repeat("ab", 32)},
		},
		{
			name:  "zero mtime",
			entry: Entry{Path: "a.txt", Size: 1, SHA256: strings.Repeat("ab", 32)},
		},
		{
			name:  "missing digest",
			entry: Entry{Path: "a.txt", Size: 1, ModTime: entryTime},
		},
		{
			name:  "truncated digest",
			entry: Entry{Path: "a.txt", Size: 1, ModTime: entryTime, SHA256: "ab"},
		},
		{
			name:  "not hex",
			entry: Entry{Path: "a.txt", Size: 1, ModTime: entryTime, SHA256: strings.Repeat("zz", 32)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManifest()
			if err := m.Add(tt.entry); err == nil {
				t.Fatalf("expected %+v to be refused", tt.entry)
			}
		})
	}
}

func TestManifestDigestIsOrderIndependent(t *testing.T) {
	forward := newTestManifest(t, "a.txt", "b.txt")
	reverse := newTestManifest(t, "b.txt", "a.txt")

	if forward.ComputeDigest() != reverse.ComputeDigest() {
		t.Fatal("two manifests with the same records must share one digest")
	}
	if forward.ComputeDigest() == NewManifest().ComputeDigest() {
		t.Fatal("an empty manifest must not share a digest with a non-empty one")
	}
}

func TestManifestDigestChangesWithRecord(t *testing.T) {
	base := testEntry("a.txt", 5)
	mutate := map[string]func(e Entry) Entry{
		"path":    func(e Entry) Entry { e.Path = "b.txt"; return e },
		"size":    func(e Entry) Entry { e.Size = 6; return e },
		"mtime":   func(e Entry) Entry { e.ModTime = e.ModTime.Add(time.Second); return e },
		"sha256":  func(e Entry) Entry { e.SHA256 = strings.Repeat("cd", 32); return e },
		"dropped": nil,
	}

	want := NewManifest()
	if err := want.Add(base); err != nil {
		t.Fatal(err)
	}
	reference := want.ComputeDigest()

	for name, change := range mutate {
		t.Run(name, func(t *testing.T) {
			m := NewManifest()
			if change == nil {
				if m.ComputeDigest() == reference {
					t.Fatal("removing a record must change the digest")
				}
				return
			}
			if err := m.Add(change(base)); err != nil {
				t.Fatal(err)
			}
			if m.ComputeDigest() == reference {
				t.Fatalf("changing the %s must change the digest", name)
			}
		})
	}
}

func TestManifestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ManifestName)

	m := NewManifest()
	if err := m.Add(testEntry("a.txt", 3)); err != nil {
		t.Fatal(err)
	}
	nested := testEntry("sub/b.bin", 5)
	nested.SHA256 = strings.Repeat("0f", 32)
	if err := m.Add(nested); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if m.Digest == "" {
		t.Fatal("WriteFile must seal the manifest digest")
	}

	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got.Schema != ManifestSchema {
		t.Fatalf("schema = %d, want %d", got.Schema, ManifestSchema)
	}
	if len(got.Files) != 2 {
		t.Fatalf("files = %+v, want 2 records", got.Files)
	}
	if got.Files[0] != testEntry("a.txt", 3) {
		t.Fatalf("round trip changed the record: %+v", got.Files[0])
	}
	if got.Files[1] != nested {
		t.Fatalf("round trip changed the record: %+v", got.Files[1])
	}
	if got.Digest != m.Digest {
		t.Fatalf("digest = %q, want %q", got.Digest, m.Digest)
	}
	if got.ComputeDigest() != got.Digest {
		t.Fatal("the stored digest must match the records it covers")
	}
	if e, ok := got.Lookup("sub/b.bin"); !ok || e.SHA256 != nested.SHA256 {
		t.Fatalf("Lookup after read = %+v, %v", e, ok)
	}

	// The JSON names are part of the public surface: a future verify command and
	// a restore check read this file, so pin them here.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		`"schema"`, `"digest"`, `"files"`,
		`"path"`, `"size"`, `"mtime"`, `"sha256"`,
		`"mtime": "2026-03-01T12:00:00.123456789Z"`,
	} {
		if !bytes.Contains(raw, []byte(needle)) {
			t.Fatalf("manifest JSON is missing %s:\n%s", needle, raw)
		}
	}
}

func TestReadFileRejectsUnknownSchema(t *testing.T) {
	tests := []struct {
		name   string
		schema int
	}{
		{name: "zero value", schema: 0},
		{name: "future version", schema: ManifestSchema + 1},
		{name: "negative", schema: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ManifestName)
			body := fmt.Sprintf(`{"schema":%d,"digest":"","files":[]}`, tt.schema)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := ReadFile(path)
			if !errors.Is(err, ErrUnknownSchema) {
				t.Fatalf("error = %v, want ErrUnknownSchema", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(tt.schema)) {
				t.Fatalf("the error must name the schema it found: %v", err)
			}
		})
	}
}

func TestReadFileAcceptsEmptySnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), ManifestName)
	m := NewManifest()
	if err := m.WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got.Files) != 0 {
		t.Fatalf("files = %+v, want none", got.Files)
	}
}

func TestReadFileRejectsBadManifest(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	record := fmt.Sprintf(`{"path":"a.txt","size":1,"mtime":"2026-03-01T12:00:00Z","sha256":%q}`, digest)

	tests := []struct {
		name string
		body string
	}{
		{name: "truncated", body: `{"schema":1,"files":[`},
		{name: "not json", body: "not a manifest"},
		{name: "empty file", body: ""},
		{name: "duplicate path", body: fmt.Sprintf(`{"schema":1,"files":[%s,%s]}`, record, record)},
		{name: "invalid path", body: fmt.Sprintf(`{"schema":1,"files":[{"path":"../x","size":1,"mtime":"2026-03-01T12:00:00Z","sha256":%q}]}`, digest)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ManifestName)
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadFile(path); err == nil {
				t.Fatalf("expected %s to be refused", tt.name)
			}
		})
	}
}

func TestReadFileWrapsMissingFile(t *testing.T) {
	_, err := ReadFile(filepath.Join(t.TempDir(), ManifestName))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want a wrapped os.ErrNotExist", err)
	}
}

func TestWriteFileRefusesUnknownSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), ManifestName)
	if err := (&Manifest{}).WriteFile(path); err == nil {
		t.Fatal("a manifest with an unknown schema must not be written")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nothing must be written, stat error: %v", err)
	}
}

func TestWriteFileLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestName)

	m := newTestManifest(t, "a.txt")
	if err := m.WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Rewriting replaces the previous manifest in one step.
	if err := newTestManifest(t, "b.txt").WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ManifestName {
		t.Fatalf("directory holds %v, want only %s", entries, ManifestName)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "b.txt" {
		t.Fatalf("files = %+v, want the replacement record", got.Files)
	}
}
