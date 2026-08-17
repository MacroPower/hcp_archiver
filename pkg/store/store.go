package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"go.jacobcolvin.com/hcp_archiver/pkg/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/pkg/serialize"
)

// Store maps a logical archive object to a stable relative path under one
// organization's archive root and commits its bytes there atomically. The root
// is joined with every relative path a builder returns, and those relative
// paths double as the opaque keys the ledger records objects under.
//
// A Store is safe for concurrent use, by two separate mechanisms. Its path
// builders are pure and a plain write delegates to atomicfile, whose
// temp-then-rename discipline lets many workers commit into the tree at once. A
// history-retaining commit ([WithHistory]) is more than that rename: it reads
// the outgoing bytes, appends them to a sidecar, and only then renames, so
// concurrent commits of one object would interleave over the sidecar. Those are
// serialized per object instead, here rather than left to the caller.
//
// The second mechanism reaches only the commits that ask for it, so a path is
// safe under concurrency while every commit of it retains history, or while
// none does. An object written both ways would let a plain rename land inside a
// history-retaining commit's critical section, between the read that captures
// the outgoing bytes and the rename that replaces them, costing the sidecar a
// generation.
//
// The serialization is also process-local, which is the whole scope that needs
// it: one archiver owns an archive root for the length of a run.
//
// Create instances with [New].
type Store struct {
	// The logger history sidecar scans report skipped records through.
	logger *slog.Logger
	// The organization's archive directory, e.g. <outputDir>/<org>.
	root string
	// Serializes the commits that touch one object's history sidecar.
	historyLocks [historyStripes]sync.Mutex
}

// Option configures a [Store] passed to [New].
//
// The available options are:
//   - [WithLogger]
type Option func(*Store)

// WithLogger sets the structured logger the history sidecars report non-fatal
// damage through (a committed record skipped because it does not parse),
// overriding [slog.Default]. A nil logger keeps the default. It returns an
// [Option].
func WithLogger(logger *slog.Logger) Option {
	return func(s *Store) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// New creates a new [Store] rooted at root.
//
// The archiver builds one Store per organization, so root is typically
// <outputDir>/<org>. The directory need not exist; write methods create any
// missing parent as they commit. The options carry what the Store cannot
// derive from its root, currently only where its history sidecars report
// damage they walk past.
func New(root string, opts ...Option) *Store {
	s := &Store{logger: slog.Default(), root: root}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// Root returns the absolute archive root the Store was created with.
func (s *Store) Root() string {
	return s.root
}

// AbsPath returns the on-disk absolute path for an archive-relative path,
// joining it beneath the root.
//
// The result is confined to the root even when relPath carries ".." segments:
// relPath is cleaned as if rooted at "/", which collapses any leading ".." that
// would otherwise escape, before being joined under the root. A legitimately
// clean archive-relative path is unchanged, so this is a boundary the write and
// read methods enforce rather than trust their callers to respect.
func (s *Store) AbsPath(relPath string) string {
	clean := cleanJoin(filepath.ToSlash(relPath))

	return filepath.Join(s.root, filepath.FromSlash(clean))
}

// Exists reports whether the object at an archive-relative path is present on
// disk.
func (s *Store) Exists(relPath string) (bool, error) {
	_, err := os.Stat(s.AbsPath(relPath))

	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("stat %q: %w", relPath, err)
	}
}

// WriteResult reports the outcome of committing an object: whether the write
// changed the on-disk bytes, and a content signature the ledger records so a
// re-run can tell "unchanged" from "updated" without diffing files.
type WriteResult struct {
	// SHA256 is the hex-encoded SHA-256 of the committed content.
	SHA256 string
	// Size is the length in bytes of the committed content.
	Size int64
	// Changed reports whether the commit altered the on-disk bytes; it is
	// false when an identical file already existed and no write was performed.
	Changed bool
}

// WriteJSON marshals v through [serialize.Marshal] and commits the result to an
// archive-relative path through [Store.WriteJSONBytes].
func (s *Store) WriteJSON(relPath string, v any) (WriteResult, error) {
	data, err := serialize.Marshal(v)
	if err != nil {
		return WriteResult{}, fmt.Errorf("marshal %q: %w", relPath, err)
	}

	return s.WriteJSONBytes(relPath, data)
}

// WriteJSONBytes commits already-marshaled JSON to an archive-relative path,
// overwriting mutable metadata only when it changes.
//
// When an identical file already exists the write is skipped and the returned
// [WriteResult] reports Changed false with the current signature; otherwise the
// bytes are committed atomically and Changed is true. The signature is computed
// over the payload either way.
//
// Under [WithHistory] a changed commit appends the outgoing content to the
// object's history sidecar before the rename, and an append that does not land
// fails the whole write with the file untouched, so a superseded version is
// never silently lost. Any commit landing over a trailing tombstone then
// closes it with the incoming content, after the bytes are durable, so the
// sidecar never records a version newer than the one the file holds. That
// closing append is the one failure the commit survives: it reports
// [ErrHistoryNotClosed] alongside a populated [WriteResult], since the bytes
// are on disk and only the sidecar lags.
func (s *Store) WriteJSONBytes(relPath string, data []byte, opts ...WriteOption) (WriteResult, error) {
	var cfg writeConfig

	for _, opt := range opts {
		opt(&cfg)
	}

	res := WriteResult{
		SHA256: sum(data),
		Size:   int64(len(data)),
	}

	// A history-retaining commit is a read-modify-write over the sidecar, so it
	// holds the object's stripe across the whole sequence below: the read of the
	// outgoing bytes, the supersede that records them, the rename, and the
	// restore that closes a tombstone. A plain commit needs none of it, the
	// rename being atomic on its own. That split rests on an object being
	// written either always with history or always without, which holds because
	// the retention is fixed per archive primitive rather than per call.
	if cfg.history {
		mu := s.historyLock(relPath)
		mu.Lock()

		defer mu.Unlock()
	}

	abs := s.AbsPath(relPath)

	//nolint:gosec // Path is composed by the store from the confined archive root.
	existing, readErr := os.ReadFile(abs)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return WriteResult{}, fmt.Errorf("read existing %q: %w", relPath, readErr)
	}

	existed := readErr == nil
	changed := !existed || !bytes.Equal(existing, data)

	if cfg.history && changed && existed {
		err := s.supersedeHistory(relPath, abs, existing, &cfg)
		if err != nil {
			return WriteResult{}, err
		}
	}

	if changed {
		err := atomicfile.WriteFile(abs, data)
		if err != nil {
			return WriteResult{}, fmt.Errorf("write %q: %w", relPath, err)
		}

		res.Changed = true
	}

	if cfg.history {
		err := s.restoreHistory(relPath, data, &cfg)
		if err != nil {
			return res, err
		}
	}

	return res, nil
}

// WriteBytes commits data to an archive-relative path atomically.
//
// It suits immutable artifacts already held in memory (policy source, small
// blobs). The write is unconditional; the returned [WriteResult] reports
// Changed true and the content signature.
func (s *Store) WriteBytes(relPath string, data []byte) (WriteResult, error) {
	err := atomicfile.WriteFile(s.AbsPath(relPath), data)
	if err != nil {
		return WriteResult{}, fmt.Errorf("write %q: %w", relPath, err)
	}

	return WriteResult{
		SHA256:  sum(data),
		Size:    int64(len(data)),
		Changed: true,
	}, nil
}

// WriteReader streams r to an archive-relative path atomically, computing the
// content signature as the bytes pass through.
//
// It suits large immutable blobs (raw state, configuration tarballs) that
// should not be buffered in memory. The write is unconditional; the returned
// [WriteResult] reports Changed true with the streamed size and hash.
func (s *Store) WriteReader(relPath string, r io.Reader) (WriteResult, error) {
	hash := sha256.New()
	counter := &countWriter{}
	tee := io.TeeReader(r, io.MultiWriter(hash, counter))

	err := atomicfile.WriteReader(s.AbsPath(relPath), tee)
	if err != nil {
		return WriteResult{}, fmt.Errorf("write %q: %w", relPath, err)
	}

	return WriteResult{
		SHA256:  hex.EncodeToString(hash.Sum(nil)),
		Size:    counter.n,
		Changed: true,
	}, nil
}

// countWriter tallies the bytes written through it, letting [Store.WriteReader]
// record a streamed blob's size without buffering it.
type countWriter struct {
	n int64
}

// Write records the length of p and reports it fully consumed.
func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))

	return len(p), nil
}

// sum returns the hex-encoded SHA-256 of data.
func sum(data []byte) string {
	h := sha256.Sum256(data)

	return hex.EncodeToString(h[:])
}
