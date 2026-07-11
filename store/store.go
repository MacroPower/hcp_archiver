package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/serialize"
)

// Store maps a logical archive object to a stable relative path under one
// organization's archive root and commits its bytes there atomically. The root
// is joined with every relative path a builder returns, and those relative
// paths double as the opaque keys the ledger records objects under.
//
// A Store is safe for concurrent use: its path builders are pure and its write
// methods delegate to atomicfile, whose temp-then-rename discipline lets many
// workers commit into the tree at once.
//
// Create instances with [New].
type Store struct {
	// The organization's archive directory, e.g. <outputDir>/<org>.
	root string
}

// New creates a new [Store] rooted at root.
//
// The archiver builds one Store per organization, so root is typically
// <outputDir>/<org>. The directory need not exist; write methods create any
// missing parent as they commit.
func New(root string) *Store {
	return &Store{root: root}
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
	clean := strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(relPath)), "/")

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
// archive-relative path, overwriting mutable metadata only when it changes.
//
// When an identical file already exists the write is skipped and the returned
// [WriteResult] reports Changed false with the current signature; otherwise the
// bytes are committed atomically and Changed is true. The signature is computed
// over the marshaled payload either way.
func (s *Store) WriteJSON(relPath string, v any) (WriteResult, error) {
	data, err := serialize.Marshal(v)
	if err != nil {
		return WriteResult{}, fmt.Errorf("marshal %q: %w", relPath, err)
	}

	res := WriteResult{
		SHA256: sum(data),
		Size:   int64(len(data)),
	}

	abs := s.AbsPath(relPath)

	//nolint:gosec // Path is composed by the store from the confined archive root.
	existing, readErr := os.ReadFile(abs)

	switch {
	case readErr == nil && bytes.Equal(existing, data):
		return res, nil
	case readErr != nil && !errors.Is(readErr, fs.ErrNotExist):
		return WriteResult{}, fmt.Errorf("read existing %q: %w", relPath, readErr)
	default:
	}

	err = atomicfile.WriteFile(abs, data)
	if err != nil {
		return WriteResult{}, fmt.Errorf("write %q: %w", relPath, err)
	}

	res.Changed = true

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
