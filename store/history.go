package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"go.jacobcolvin.com/hcp_archiver/history"
)

// writeConfig holds the resolved settings for a single JSON commit.
type writeConfig struct {
	fetchedAt time.Time
	now       time.Time
	history   bool
}

// WriteOption configures a [Store.WriteJSONBytes] commit.
//
// The available options are:
//   - [WithHistory]
type WriteOption func(*writeConfig)

// WithHistory retains the object's history across the commit: a changed
// write appends the outgoing content to the object's sidecar before the new
// bytes rename into place, and a commit landing over a trailing tombstone
// appends the incoming content to close the recorded deletion, whether or
// not the bytes changed. The superseded content is stamped fetchedAt (the
// prior run's fetch time; a zero value falls back to the file's modification
// time, then to omitted) and a reappearance is stamped now. It returns a
// [WriteOption].
func WithHistory(fetchedAt, now time.Time) WriteOption {
	return func(c *writeConfig) {
		c.history = true
		c.fetchedAt = fetchedAt
		c.now = now
	}
}

// HistoryPath returns the archive-relative path of the history sidecar
// retaining superseded versions of the object at relPath (see
// [history.Path]).
func (s *Store) HistoryPath(relPath string) string {
	return history.Path(relPath)
}

// BuryHistory records an observed disappearance of the mutable object at
// relPath: the last-known file content is flushed into the history sidecar
// (unless its newest record already carries it) and a tombstone stamped
// deletedAt is appended, exactly once per disappearance however often the
// absence re-probes. The object file itself is left in place; nothing local
// is ever removed. The flushed content is stamped fetchedAt, falling back to
// the file's modification time when zero. It reports whether anything was
// appended; the report is meaningful only when err is nil.
func (s *Store) BuryHistory(relPath string, fetchedAt, deletedAt time.Time) (bool, error) {
	abs := s.AbsPath(relPath)

	//nolint:gosec // Path is composed by the store from the confined archive root.
	content, err := os.ReadFile(abs)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		content = nil
	case err != nil:
		return false, fmt.Errorf("read %q to bury: %w", relPath, err)
	}

	// A zero-length file carries no version worth preserving, so treat it
	// like an absent file: [history.Bury] still lands the tombstone when a
	// sidecar exists.
	if len(content) == 0 {
		content = nil
	}

	if fetchedAt.IsZero() && content != nil {
		fetchedAt = modTime(abs)
	}

	buried, err := history.Bury(s.AbsPath(s.HistoryPath(relPath)), content, fetchedAt, deletedAt)
	if err != nil {
		return false, fmt.Errorf("retain history %q: %w", relPath, err)
	}

	return buried, nil
}

// retainHistory runs the history side of one [Store.WriteJSONBytes] commit
// (see [WithHistory]): when supersede is set (a changed write over an
// existing file with content) the outgoing content is appended first, then any
// trailing tombstone is closed with the incoming content. Every error returns
// before the caller renames, so a version is never lost to a write that could
// not record it.
func (s *Store) retainHistory(
	relPath, abs string,
	existing, data []byte,
	supersede bool,
	cfg *writeConfig,
) error {
	sidecar := s.AbsPath(s.HistoryPath(relPath))

	// A zero-length file carries no version worth preserving, the same reading
	// [Store.BuryHistory] takes. Superseding one would append a record whose
	// content marshals away under omitempty, leaving a line readers can
	// classify as neither a version nor a tombstone.
	if supersede && len(existing) > 0 {
		fetchedAt := cfg.fetchedAt
		if fetchedAt.IsZero() {
			fetchedAt = modTime(abs)
		}

		_, err := history.Supersede(sidecar, existing, fetchedAt)
		if err != nil {
			return fmt.Errorf("retain history %q: %w", relPath, err)
		}
	}

	_, err := history.Restore(sidecar, data, cfg.now)
	if err != nil {
		return fmt.Errorf("retain history %q: %w", relPath, err)
	}

	return nil
}

// modTime returns the file's modification time, or the zero time when it
// cannot be read: the fallback stamp for superseded content whose fetch time
// no ledger entry could supply (a rebuilt ledger).
func modTime(abs string) time.Time {
	info, err := os.Stat(abs)
	if err != nil {
		return time.Time{}
	}

	return info.ModTime()
}
