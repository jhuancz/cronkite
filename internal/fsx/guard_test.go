package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newGuard(t *testing.T, roots, protect []string) *Guard {
	t.Helper()
	g, err := NewGuard(roots, protect)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

func TestRemoveInsideRoot(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.log")
	write(t, file, "x")

	g := newGuard(t, []string{root}, nil)
	if err := g.Remove(file); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("file should be gone, stat error: %v", err)
	}
}

func TestRemoveOutsideRootIsRefused(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "other.log")
	write(t, outside, "x")

	g := newGuard(t, []string{root}, nil)
	err := g.Remove(outside)
	if err == nil {
		t.Fatal("expected a refusal for a path outside every root")
	}
	if _, statErr := os.Stat(outside); statErr != nil {
		t.Fatalf("the outside file must survive: %v", statErr)
	}
}

// TestSymlinkCannotEscape is the safety property the whole package exists for.
func TestSymlinkCannotEscape(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	write(t, secret, "do not delete me")

	root := t.TempDir()
	link := filepath.Join(root, "link-to-outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	g := newGuard(t, []string{root}, nil)

	// Walking must not descend into the symlink: the only entry seen is the
	// link itself, and the secret behind it is never reached.
	var seen []string
	if err := g.Roots()[0].Walk(0, func(abs, _ string, d fs.DirEntry) error {
		seen = append(seen, filepath.Base(abs))
		return nil
	}, nil); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, name := range seen {
		if name == "secret.txt" {
			t.Fatalf("walk crossed the symlink and saw %q", name)
		}
	}
	if len(seen) != 1 || seen[0] != "link-to-outside" {
		t.Fatalf("walk should see only the link, saw %v", seen)
	}

	// Removing the link removes the link, never its target.
	if err := g.Remove(link); err != nil {
		t.Fatalf("Remove(link): %v", err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("the symlink target must survive: %v", err)
	}

	// The target itself is outside the root and cannot be reached.
	if err := g.Remove(secret); err == nil {
		t.Fatal("expected a refusal for the symlink target")
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("the symlink target must survive: %v", err)
	}
}

func TestProtectedCoversSubtree(t *testing.T) {
	root := t.TempDir()
	kept := filepath.Join(root, "keepdir", "nested")
	write(t, filepath.Join(kept, "old.log"), "x")

	g := newGuard(t, []string{root}, []string{"keep*"})
	if err := g.Remove(filepath.Join(kept, "old.log")); !errors.Is(err, ErrProtected) {
		t.Fatalf("expected ErrProtected, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(kept, "old.log")); err != nil {
		t.Fatalf("protected file must survive: %v", err)
	}
}

func TestProtectMatchesTrailingPath(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "logs", "cronkite", "run.log"), "x")

	g := newGuard(t, []string{root}, []string{"cronkite/*"})
	if !g.Protected(filepath.Join(root, "logs", "cronkite", "run.log")) {
		t.Fatal("expected cronkite/* to protect a nested path")
	}
	if g.Protected(filepath.Join(root, "logs", "other.log")) {
		t.Fatal("unrelated path should not be protected")
	}
}

func TestMissingRootIsReportedNotFatal(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "does-not-exist")

	g := newGuard(t, []string{root, missing}, nil)
	if len(g.Roots()) != 1 {
		t.Fatalf("roots = %d, want 1", len(g.Roots()))
	}
	if len(g.Missing()) != 1 || g.Missing()[0] != missing {
		t.Fatalf("missing = %v, want [%s]", g.Missing(), missing)
	}
}

func TestNestedRootIsDeduplicated(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	g := newGuard(t, []string{root, nested, root}, nil)
	if len(g.Roots()) != 1 {
		t.Fatalf("roots = %d, want 1 (nested and duplicate roots add nothing)", len(g.Roots()))
	}
}

func TestRootItselfCannotBeRemoved(t *testing.T) {
	root := t.TempDir()
	g := newGuard(t, []string{root}, nil)
	if err := g.Remove(root); err == nil {
		t.Fatal("the root itself must not be a removal target")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root must survive: %v", err)
	}
}

func TestRemoveThroughSymlinkedAncestor(t *testing.T) {
	real := t.TempDir()
	linkParent := filepath.Join(t.TempDir(), "via-link")
	if err := os.Symlink(real, linkParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	file := filepath.Join(real, "old.log")
	write(t, file, "x")

	g := newGuard(t, []string{real}, nil)
	// Addressed through the symlinked ancestor, as an operator would.
	viaLink := filepath.Join(linkParent, "old.log")
	if err := g.Remove(viaLink); err != nil {
		t.Fatalf("Remove through a symlinked ancestor: %v", err)
	}
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("file should be gone, stat error: %v", err)
	}
}

func TestWalkHonoursMaxDepth(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "top.log"), "x")
	write(t, filepath.Join(root, "lvl1", "a.log"), "x")
	write(t, filepath.Join(root, "lvl1", "lvl2", "b.log"), "x")

	var seen []string
	g := newGuard(t, []string{root}, nil)
	if err := g.Roots()[0].Walk(2, func(abs, _ string, d fs.DirEntry) error {
		if !d.IsDir() {
			seen = append(seen, filepath.Base(abs))
		}
		return nil
	}, nil); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	want := map[string]bool{"top.log": true, "a.log": true}
	for _, name := range seen {
		if !want[name] {
			t.Errorf("maxDepth 2 should not reach %q (saw %v)", name, seen)
		}
	}
	if len(seen) != 2 {
		t.Errorf("saw %d files, want 2: %v", len(seen), seen)
	}
}
