package fsid

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Canonical resolves path to the identity of the physical location it
// denotes: two paths that reach one on-disk directory — a relocation or
// rename symlink and its target, say — map to one string, fit for use as a
// map key.
//
// The path need not exist. Resolution walks up to the deepest existing
// ancestor, resolves that, and rejoins the missing remainder, so a directory
// about to be created beneath a symlinked parent aliases its physical
// location before anything exists at the path. A resolution fault other than
// absence falls back to the cleaned path: the result is an identity, not an
// I/O handle, and a degraded identity only weakens alias detection to the
// plain path equality the caller started with.
func Canonical(path string) string {
	path = filepath.Clean(path)
	probe := path

	var tail []string

	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			return filepath.Join(append([]string{resolved}, tail...)...)
		}

		if !errors.Is(err, fs.ErrNotExist) {
			return path
		}

		parent := filepath.Dir(probe)
		if parent == probe {
			return path
		}

		tail = append([]string{filepath.Base(probe)}, tail...)
		probe = parent
	}
}

// WalkFiles hands fn the logical path of every regular file beneath root and
// returns the intra-tree directory aliases it declined to follow, keyed
// alias logical path to owning logical path.
//
// The naming rule: a file physically inside a walked tree always reports
// under its physical name. A symlinked directory whose target the traversal
// already owns — a rename alias beside its target, a link cycle — is not
// followed; it is returned as an alias instead, so a caller reconciling
// names minted before a rename can rewrite between them. Every other name in
// the system (ledger entries, remote keys) derives from the physical layout,
// so the walk must not let a link's name shadow it, which would depend on
// lexical visit order. A symlinked directory whose target lies outside every
// walked tree — an operator relocation — is followed, its files reported at
// their logical location under the link, and its target joins the owned
// trees.
//
// Containment cuts both ways: a followed target that itself contains an
// already-owned tree (nested relocation links, a link to an ancestor of the
// root) surfaces that tree as a plain directory mid-walk, and the traversal
// declines it there with the same alias bookkeeping, so no visit order can
// walk a physical directory twice.
//
// A symlink to a regular file reports like the file, a dangling link is
// skipped, and any other stat or walk fault propagates, so a fault cannot
// silently hide a subtree from the caller. The walk stops between entries
// once ctx ends.
func WalkFiles(ctx context.Context, root string, fn func(logical string) error) (map[string]string, error) {
	w := &walker{aliases: make(map[string]string), fn: fn}

	// The root resolves up front, so a walk rooted at a symlink (a relocated
	// subtree walked directly) traverses its physical tree while reporting
	// under the requested logical root.
	err := w.walk(ctx, Canonical(root), root)

	return w.aliases, err
}

// walker carries one [WalkFiles] traversal: the physical subtrees being
// walked, each mapped to the logical path it reports under, and the
// intra-tree aliases declined along the way.
type walker struct {
	fn      func(logical string) error
	aliases map[string]string
	roots   []walkRoot
}

// walkRoot pairs one walked physical subtree with the logical path its files
// report under; the two differ only beneath a followed relocation link.
type walkRoot struct {
	phys    string
	logical string
}

// walk traverses the physical directory physical, reporting each file at its
// logical location under logical, and registers the pair so a link back into
// this subtree reads as an alias rather than a second traversal.
func (w *walker) walk(ctx context.Context, physical, logical string) error {
	w.roots = append(w.roots, walkRoot{phys: physical, logical: logical})

	//nolint:wrapcheck // Each fault below is wrapped with its path; the walk's own errors carry theirs.
	return filepath.WalkDir(physical, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		case d.IsDir():
			if p == physical {
				return nil
			}

			// A followed link's target can physically contain a tree another
			// walk already owns; it surfaces here as a plain directory whose
			// path IS a registered root. Decline it like the symlinked case,
			// or the subtree would report twice under two logical names.
			owner, owned := w.rootAt(p)
			if !owned {
				return nil
			}

			lp, lpErr := logicalPath(physical, logical, p)
			if lpErr != nil {
				return lpErr
			}

			w.aliases[lp] = owner

			return fs.SkipDir
		}

		lp, lpErr := logicalPath(physical, logical, p)
		if lpErr != nil {
			return lpErr
		}

		if d.Type()&fs.ModeSymlink != 0 {
			return w.symlinked(ctx, p, lp)
		}

		if !d.Type().IsRegular() {
			return nil
		}

		return w.fn(lp)
	})
}

// symlinked resolves one symlinked walk entry with [os.Stat]. A link to a
// regular file reports like the file, a dangling link reads as absent, and
// any other stat fault surfaces. A link to a directory splits on where the
// target lives: inside a walked tree it records an alias (see [WalkFiles]),
// outside it starts a nested walk through the link.
func (w *walker) symlinked(ctx context.Context, physical, logical string) error {
	info, err := os.Stat(physical)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("stat symlinked entry %q: %w", physical, err)
	case info.Mode().IsRegular():
		return w.fn(logical)
	case !info.IsDir():
		return nil
	}

	resolved, err := filepath.EvalSymlinks(physical)
	if err != nil {
		return fmt.Errorf("resolve symlinked directory %q: %w", physical, err)
	}

	if owner, ok := w.ownerOf(resolved); ok {
		w.aliases[logical] = owner

		return nil
	}

	return w.walk(ctx, resolved, logical)
}

// ownerOf maps a resolved directory to the logical path the traversal
// reports it under, when a subtree already being walked contains it.
func (w *walker) ownerOf(resolved string) (string, bool) {
	for _, r := range w.roots {
		if resolved == r.phys {
			return r.logical, true
		}

		if strings.HasPrefix(resolved, r.phys+string(filepath.Separator)) {
			return filepath.Join(r.logical, resolved[len(r.phys):]), true
		}
	}

	return "", false
}

// rootAt maps a physically reached directory to the logical root another
// walk owns it under, when the directory is itself a registered walk root:
// the containment direction [walker.ownerOf] cannot see, since mid-walk an
// owned tree always surfaces as an exact root match rather than a resolved
// link target.
func (w *walker) rootAt(p string) (string, bool) {
	for _, r := range w.roots {
		if p == r.phys {
			return r.logical, true
		}
	}

	return "", false
}

// logicalPath translates a path reported by a physical walk rooted at
// physical to its logical location under logical; the two roots are equal
// everywhere except beneath a followed symlink.
func logicalPath(physical, logical, p string) (string, error) {
	if physical == logical {
		return p, nil
	}

	rel, err := filepath.Rel(physical, p)
	if err != nil {
		return "", fmt.Errorf("relativize %q: %w", p, err)
	}

	return filepath.Join(logical, rel), nil
}
