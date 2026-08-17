package store

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"go.jacobcolvin.com/hcp_archiver/pkg/history"
)

// historyStripes is how many mutexes the commits that touch a history sidecar
// are spread across.
//
// A sidecar is appended to by a read-modify-write ([history.Supersede] scans
// the committed tail before it appends, and the append trims any uncommitted
// fragment and writes at the boundary it computed), so two commits of one
// object must not overlap: they would compute that boundary independently. A fixed
// stripe array rather than a mutex per path: an archive holds a mutable object
// for every run of every workspace, so a map keyed by path would grow with the
// tree to hold a lock that is almost never contended. Two objects landing on
// one stripe only serialize a pair of local writes.
const historyStripes = 256

// ErrHistoryNotClosed indicates that a commit's bytes landed but the record
// closing a trailing tombstone in the object's history sidecar did not. It
// accompanies a populated [WriteResult], because the object file is committed
// and durable; only the sidecar's agreement with it is outstanding, and the
// next commit re-attempts the close. A caller should record the object done
// and surface the cause, never treat it as unwritten.
var ErrHistoryNotClosed = errors.New("history tombstone not closed")

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
// appends the incoming content afterwards to close the recorded deletion,
// whether or not the bytes changed. The superseded content is stamped
// fetchedAt (the prior run's fetch time; a zero value falls back to the
// file's modification time, then to omitted) and a reappearance is stamped
// now.
//
// Commits carrying it are serialized per object, since appending to a sidecar
// reads it first and so cannot be made atomic the way the object file's rename
// is. Concurrent commits of one object are therefore safe. A commit without it
// takes no such lock, so the serialization covers a path only while every
// commit of it retains history, which holds because retention is fixed per
// archive primitive rather than per call. It returns a [WriteOption].
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

// historyLock returns the mutex serializing the commits that touch the history
// sidecar of the object at relPath, one of historyStripes.
//
// It keys on the absolute sidecar path, the identical string the appends
// themselves open, so the lock and the file it protects are named the same way.
// The sidecar rather than the object because [history.Path] trims a trailing
// ".json", and the absolute form because [Store.AbsPath] cleans as it joins:
// either mapping is many-to-one, and hashing ahead of it would let two names
// for one sidecar past each other.
func (s *Store) historyLock(relPath string) *sync.Mutex {
	h := fnv.New32a()

	//nolint:gosec // hash.Hash never reports a write error.
	h.Write([]byte(s.AbsPath(s.HistoryPath(relPath))))

	return &s.historyLocks[h.Sum32()%historyStripes]
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
	// The other door onto the sidecar, held against a concurrent commit of the
	// same object for the same reason a write holds it.
	mu := s.historyLock(relPath)
	mu.Lock()

	defer mu.Unlock()

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

	// Content rotted past valid UTF-8 is unrecordable for the reason
	// [Store.supersedeHistory] gives; treat it like an absent file rather
	// than let a rotted object block its own disappearance from recording.
	if !utf8.Valid(content) {
		s.logger.Warn("history_content_not_utf8",
			slog.String("path", relPath),
		)

		content = nil
	}

	if fetchedAt.IsZero() && content != nil {
		fetchedAt = modTime(abs)
	}

	buried, err := history.Bury(
		s.AbsPath(s.HistoryPath(relPath)), content, fetchedAt, deletedAt,
		history.WithLogger(s.logger),
	)
	if err != nil {
		return false, fmt.Errorf("retain history %q: %w", relPath, err)
	}

	return buried, nil
}

// supersedeHistory appends the content a changed [Store.WriteJSONBytes]
// commit is about to overwrite (see [WithHistory]). It runs before the rename
// and returns its error to the caller, so a version is never lost to a write
// that could not record it; a rename that then fails costs nothing, because
// the appended record still holds exactly what the file still holds and the
// next commit dedupes against it.
func (s *Store) supersedeHistory(relPath, abs string, existing []byte, cfg *writeConfig) error {
	// A zero-length file carries no version worth preserving, the same reading
	// [Store.BuryHistory] takes. Superseding one would append a record whose
	// content marshals away under omitempty, leaving a line readers can
	// classify as neither a version nor a tombstone.
	if len(existing) == 0 {
		return nil
	}

	// Bytes rotted past valid UTF-8 cannot be recorded faithfully (a record
	// carries content as a JSON string), and [history.Supersede]'s refusal
	// would wedge every future commit of the object behind a version that is
	// already lost. Skip the record and let the fresh bytes land, warning the
	// way any other unrecoverable sidecar content is warned about.
	if !utf8.Valid(existing) {
		s.logger.Warn("history_content_not_utf8",
			slog.String("path", relPath),
		)

		return nil
	}

	fetchedAt := cfg.fetchedAt
	if fetchedAt.IsZero() {
		fetchedAt = modTime(abs)
	}

	_, err := history.Supersede(s.AbsPath(s.HistoryPath(relPath)), existing, fetchedAt,
		history.WithLogger(s.logger))
	if err != nil {
		return fmt.Errorf("retain history %q: %w", relPath, err)
	}

	return nil
}

// restoreHistory closes a trailing tombstone with the content the commit just
// landed (see [WithHistory]).
//
// It runs after the object file holds data, never before, because the
// sidecar's newest version must never be one the file does not yet hold: a
// record appended ahead of a rename that then failed would leave the next
// commit superseding the file's older content on top of it, ordering the
// timeline backward.
//
// Appending afterwards risks no version, since the content is already durable
// in the object file itself. The record is a consistency marker rather than
// the only copy of anything, so its failure reports [ErrHistoryNotClosed] and
// leaves the commit standing: the next commit re-attempts the close, and
// should the object disappear first, [history.Bury] flushes the unrecorded
// content ahead of its tombstone.
func (s *Store) restoreHistory(relPath string, data []byte, cfg *writeConfig) error {
	_, err := history.Restore(s.AbsPath(s.HistoryPath(relPath)), data, cfg.now,
		history.WithLogger(s.logger))
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrHistoryNotClosed, relPath, err)
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
