// Package fsx confines every destructive operation to a configured root.
//
// The safety property is not a convention here, it is enforced by the kernel
// path resolution of os.Root: a relative handle cannot traverse ".." or follow
// a symlink out of its tree. A configured root is therefore a hard boundary,
// and cronkite cannot be tricked into deleting something outside it even if a
// symlink inside the tree points at /etc.
package fsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ErrProtected marks a path excluded by a "protect" pattern.
var ErrProtected = errors.New("protected by configuration")

// Root is a path-confined handle on one configured tree.
type Root struct {
	dir string
	h   *os.Root
}

// OpenRoot resolves dir, requires it to exist, and opens it as a confinement
// boundary.
func OpenRoot(dir string) (*Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve root %q: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve root %q: %w", dir, err)
	}
	h, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("open root %q: %w", dir, err)
	}
	return &Root{dir: resolved, h: h}, nil
}

// Dir is the resolved absolute path of the root.
func (r *Root) Dir() string { return r.dir }

// Close releases the handle.
func (r *Root) Close() error { return r.h.Close() }

// Rel maps an absolute path into a root-relative path. It reports false when
// the path is not strictly inside the root.
//
// The comparison falls back to resolving the path's parent directory, because
// operators address files through symlinked ancestors all the time (on macOS
// /var is /private/var, and containers do the same with /tmp). Resolving the
// parent and keeping the base name is deliberate: given a path that is itself a
// symlink, the target must stay the link, never what it points at.
func (r *Root) Rel(abs string) (string, bool) {
	if rel, ok := relWithin(r.dir, abs); ok {
		return rel, true
	}
	parent, base := filepath.Split(filepath.Clean(abs))
	if base == "" {
		return "", false
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Clean(parent))
	if err != nil {
		return "", false
	}
	return relWithin(r.dir, filepath.Join(resolvedParent, base))
}

func relWithin(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// Remove deletes one entry through the confinement handle. Directories are
// only removed when empty.
func (r *Root) Remove(rel string) error { return r.h.Remove(rel) }

// WalkFunc receives one entry. rel is root-relative and never ".".
type WalkFunc func(abs, rel string, d fs.DirEntry) error

// Walk traverses the root without following symlinks. maxDepth limits how many
// directory levels below the root are descended; 0 means no limit. Read errors
// are reported to onErr and skipped rather than aborting the whole run, because
// one unreadable subdirectory must not hide every other candidate.
func (r *Root) Walk(maxDepth int, visit WalkFunc, onErr func(abs string, err error)) error {
	sep := string(filepath.Separator)
	return filepath.WalkDir(r.dir, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			if onErr != nil {
				onErr(abs, err)
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, ok := r.Rel(abs)
		if !ok {
			// The root itself.
			return nil
		}

		depth := strings.Count(rel, sep) + 1
		if d.IsDir() && maxDepth > 0 && depth >= maxDepth {
			if visit != nil {
				if err := visit(abs, rel, d); err != nil {
					return err
				}
			}
			return filepath.SkipDir
		}
		if visit == nil {
			return nil
		}
		return visit(abs, rel, d)
	})
}

// Guard owns every configured root plus the protect patterns.
type Guard struct {
	roots   []*Root
	missing []string
	protect []string
}

// NewGuard opens every root that exists. Roots that do not exist are reported
// through Missing instead of failing, so one absent optional directory does not
// block a run on another platform.
func NewGuard(roots, protect []string) (*Guard, error) {
	g := &Guard{protect: protect}
	seen := map[string]bool{}
	for _, dir := range roots {
		root, err := OpenRoot(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
				g.missing = append(g.missing, dir)
				continue
			}
			g.Close()
			return nil, err
		}
		if seen[root.Dir()] {
			root.Close()
			continue
		}
		// A nested root adds nothing: the parent walk already reaches it.
		if covered(g.roots, root.Dir()) {
			root.Close()
			continue
		}
		seen[root.Dir()] = true
		g.roots = append(g.roots, root)
	}
	return g, nil
}

// Close releases every root handle.
func (g *Guard) Close() error {
	var firstErr error
	for _, r := range g.roots {
		if err := r.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	g.roots = nil
	return firstErr
}

// Roots returns the opened roots.
func (g *Guard) Roots() []*Root { return g.roots }

// Missing returns configured roots that did not exist when the guard was built.
func (g *Guard) Missing() []string { return g.missing }

// Remove deletes abs if and only if it sits inside a configured root and is not
// protected.
func (g *Guard) Remove(abs string) error {
	for _, r := range g.roots {
		rel, ok := r.Rel(abs)
		if !ok {
			continue
		}
		if g.Protected(abs) {
			return ErrProtected
		}
		return r.Remove(rel)
	}
	return fmt.Errorf("path %q is outside every configured root", abs)
}

// Protected reports whether abs, or any of its ancestors, matches a protect
// pattern. Protecting a directory therefore protects its whole subtree.
func (g *Guard) Protected(abs string) bool {
	if len(g.protect) == 0 {
		return false
	}
	p := filepath.ToSlash(filepath.Clean(abs))
	for {
		if g.matches(p) {
			return true
		}
		parent := path.Dir(p)
		if parent == p || strings.HasSuffix(parent, ":") || parent == "." {
			return false
		}
		p = parent
	}
}

// matches accepts a bare name pattern against the base name, and a pattern with
// a separator against the full path or any trailing part of it. That makes
// "cronkite/*" protect /var/log/cronkite today and /srv/x/cronkite tomorrow.
func (g *Guard) matches(p string) bool {
	base := path.Base(p)
	for _, pattern := range g.protect {
		pat := filepath.ToSlash(pattern)
		if ok, err := path.Match(pat, base); err == nil && ok {
			return true
		}
		if !strings.Contains(pat, "/") {
			continue
		}
		for suffix := p; ; {
			if ok, err := path.Match(pat, suffix); err == nil && ok {
				return true
			}
			idx := strings.IndexByte(suffix, '/')
			if idx < 0 {
				break
			}
			suffix = suffix[idx+1:]
		}
	}
	return false
}

func covered(roots []*Root, dir string) bool {
	for _, r := range roots {
		if dir == r.dir {
			return true
		}
		if strings.HasPrefix(dir, r.dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
