package fsid

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

// WalkFiles hands fn the logical path of every regular file beneath root,
// following symlinked directories.
//
// Every physical directory is visited exactly once, under whichever name the
// walk reaches first, so a subtree reachable both directly and through a
// sibling symlink yields its files once, and a link cycle terminates. A file
// reached through a followed link is reported at its logical location under
// the link, so reported paths stay rooted beneath root. A symlink to a
// regular file reports like the file, a dangling link is skipped, a directory
// that vanishes mid-walk is skipped, and any other stat or walk fault
// propagates, so a fault cannot silently hide a subtree from the caller. The
// walk stops between entries once ctx ends.
func WalkFiles(ctx context.Context, root string, fn func(logical string) error) error {
	return walkFiles(ctx, root, root, make(map[string]struct{}), fn)
}

// walkFiles walks the physical directory physical for [WalkFiles], reporting
// each file at its logical location under logical. The two roots differ only
// beneath a followed symlink, where the walk descends the link's resolved
// target while every reported path stays under the link itself.
//
// Every directory registers its physical identity in visited on entry, and a
// directory whose identity is already registered is skipped whole. The same
// set breaks symlink cycles (see [walkSymlinked]).
func walkFiles(
	ctx context.Context,
	physical, logical string,
	visited map[string]struct{},
	fn func(logical string) error,
) error {
	//nolint:wrapcheck // Each fault below is wrapped with its path; the walk's own errors carry theirs.
	return filepath.WalkDir(physical, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		case d.IsDir():
			resolved, rErr := filepath.EvalSymlinks(p)

			switch {
			case errors.Is(rErr, fs.ErrNotExist):
				// The directory vanished mid-walk; nothing beneath it remains
				// to visit.
				return fs.SkipDir
			case rErr != nil:
				return fmt.Errorf("resolve directory %q: %w", p, rErr)
			}

			if _, seen := visited[resolved]; seen {
				return fs.SkipDir
			}

			visited[resolved] = struct{}{}

			return nil
		}

		lp, lpErr := logicalPath(physical, logical, p)
		if lpErr != nil {
			return lpErr
		}

		if d.Type()&fs.ModeSymlink != 0 {
			return walkSymlinked(ctx, p, lp, visited, fn)
		}

		if !d.Type().IsRegular() {
			return nil
		}

		return fn(lp)
	})
}

// walkSymlinked resolves one symlinked walk entry with [os.Stat]. A link to a
// regular file reports like the file, a link to a directory is walked through
// the link (each physical directory once — the recursion's root visit
// registers it in visited, which also breaks link cycles), a dangling link
// reads as absent, and any other stat fault surfaces.
func walkSymlinked(
	ctx context.Context,
	physical, logical string,
	visited map[string]struct{},
	fn func(logical string) error,
) error {
	info, err := os.Stat(physical)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("stat symlinked entry %q: %w", physical, err)
	case info.Mode().IsRegular():
		return fn(logical)
	case !info.IsDir():
		return nil
	}

	resolved, err := filepath.EvalSymlinks(physical)
	if err != nil {
		return fmt.Errorf("resolve symlinked directory %q: %w", physical, err)
	}

	if _, ok := visited[resolved]; ok {
		return nil
	}

	return walkFiles(ctx, resolved, logical, visited, fn)
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
