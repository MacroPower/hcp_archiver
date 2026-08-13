package view

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"go.jacobcolvin.com/hcp_archiver/store"
)

var (
	// ErrObjectNotFound indicates an archive-relative path is present in none of
	// the archive's physical forms, with no eviction stub standing in for it
	// either.
	//
	// The two evicted surfaces answer differently, because only one of them
	// leaves a stub. An evicted configuration tarball is fetched from the
	// mirror or reported [ErrRemoteOnly]; a member of an evicted bundle in an
	// organization recording no mirror reports here, since the sidecar that
	// lists it is not something its bytes can be read out of.
	ErrObjectNotFound = errors.New("object not found in archive")

	// ErrRollupChecksum indicates a roll-up member's content does not hash to
	// its recorded digest.
	ErrRollupChecksum = errors.New("roll-up member does not match its recorded checksum")
)

// Workspace is a read handle on one workspace's subtree.
//
// Its lookups are transparent to sealing: [Workspace.Open] finds an object
// whether it is still a loose file, coalesced into a roll-up, or packed into a
// bundle. Instances are produced by [*Org.Workspace].
type Workspace struct {
	org     *Org
	idx     map[string]sealedRef
	Project string
	Name    string
	dir     string
	mu      sync.Mutex
}

// sealedRef locates one sealed object: the roll-up line or bundle member that
// carries its bytes. A roll-up is named by its absolute path (roll-ups are
// always local), while a bundle is named by its archive-relative path, so one
// value resolves to either the local zip or the remote object it was evicted
// to.
type sealedRef struct {
	rollup string
	bundle string
	offset int64
	size   int64
}

// rollupLine mirrors one roll-up record: the member's archive-relative path,
// the SHA-256 the sealer recorded over its bytes, and the content carried
// verbatim as a JSON string.
type rollupLine struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

// sidecarLine mirrors one bundle sidecar record: the member's archive-relative
// path, the bundle that holds it, and its content length.
type sidecarLine struct {
	Name   string `json:"name"`
	Bundle string `json:"bundle"`
	Size   int64  `json:"size"`
}

// Dir returns the workspace's archive-relative directory.
func (w *Workspace) Dir() string {
	return w.dir
}

// File returns the archive-relative path of a leaf named name directly under
// the workspace's directory.
func (w *Workspace) File(name string) string {
	return path.Join(w.dir, name)
}

// Open reads the object at an archive-relative path, looking first for a loose
// file, then in the workspace's roll-ups, then in its bundles. A path present
// in no form returns [ErrObjectNotFound].
//
// The loose file wins when both exist: sealing removes a loose source only
// after its sealed copy verifies, so a survivor from an interrupted seal is the
// canonical copy.
func (w *Workspace) Open(relPath string) ([]byte, error) {
	data, err := os.ReadFile(w.org.AbsPath(relPath))

	switch {
	case err == nil:
		return data, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("read %q: %w", relPath, err)
	}

	return w.openSealed(relPath)
}

// openSealed reads the object at an archive-relative path from the workspace's
// sealed forms alone, the half of [Workspace.Open] that runs once no loose file
// answers. [Workspace.writeObject] shares it, so the two agree on which sealed
// form carries a path and on what its absence means.
func (w *Workspace) openSealed(relPath string) ([]byte, error) {
	idx, err := w.index()
	if err != nil {
		return nil, err
	}

	ref, ok := idx[relPath]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, relPath)
	}

	if ref.rollup != "" {
		return readRollupLine(ref.rollup, ref.offset, relPath)
	}

	return w.readBundleMember(ref.bundle, relPath)
}

// readBundleMember extracts one member from the bundle at an archive-relative
// path, preferring the local zip; a zip evicted to the remote store is read
// through ranged GETs instead. The local copy wins when both exist, matching
// the loose-file rule: until eviction confirms the remote copy, the local
// bundle is canonical.
func (w *Workspace) readBundleMember(relBundle, relPath string) ([]byte, error) {
	data, err := readLocalBundleMember(w.org.AbsPath(relBundle), relPath)

	switch {
	case err == nil:
		return data, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}

	if w.org.remote == nil {
		return nil, fmt.Errorf("%w: %s (bundle %q is not on disk and no remote is configured)",
			ErrObjectNotFound, relPath, relBundle)
	}

	return w.org.remote.readMember(relBundle, relPath)
}

// Exists reports whether the object at an archive-relative path is present in
// any physical form.
func (w *Workspace) Exists(relPath string) (bool, error) {
	_, err := os.Stat(w.org.AbsPath(relPath))

	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("stat %q: %w", relPath, err)
	}

	idx, err := w.index()
	if err != nil {
		return false, err
	}

	_, ok := idx[relPath]

	return ok, nil
}

// sealedNames returns the archive-relative paths of the workspace's sealed
// objects strictly beneath an archive-relative directory prefix. A key equal
// to the prefix itself (a corrupt record naming a directory) is dropped: the
// callers derive child names from what follows the prefix.
func (w *Workspace) sealedNames(dirPrefix string) ([]string, error) {
	sealed, err := w.sealedUnder(dirPrefix)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(sealed))

	for _, se := range sealed {
		if se.rel != dirPrefix {
			names = append(names, se.rel)
		}
	}

	return names, nil
}

// sealedEntry pairs one sealed object's archive-relative path with the
// reference locating its bytes.
type sealedEntry struct {
	rel string
	ref sealedRef
}

// sealedUnder returns the workspace's sealed objects at or beneath an
// archive-relative path prefix. An empty prefix returns every sealed object.
func (w *Workspace) sealedUnder(prefix string) ([]sealedEntry, error) {
	idx, err := w.index()
	if err != nil {
		return nil, err
	}

	var entries []sealedEntry

	for rel, ref := range idx {
		if underPrefix(rel, prefix) {
			entries = append(entries, sealedEntry{rel: rel, ref: ref})
		}
	}

	return entries, nil
}

// underPrefix reports whether a slash-separated path sits at or beneath a
// prefix, matching whole segments so "runs/run-1" never matches "runs/run-10".
func underPrefix(p, prefix string) bool {
	return prefix == "" || p == prefix || strings.HasPrefix(p, prefix+"/")
}

// index returns the workspace's sealed-object index, building it on first use
// from the roll-ups and bundle sidecars. Keys are archive-relative paths, the
// same keys the ledger records, so a lookup is exact.
func (w *Workspace) index() (map[string]sealedRef, error) {
	// Bubble Tea runs its commands on separate goroutines, so two descents into
	// the same workspace can build the index at once. Serialize the whole
	// check-build-store so the lazy field is written under the lock rather than
	// raced on.
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.idx != nil {
		return w.idx, nil
	}

	idx := make(map[string]sealedRef)

	err := w.indexRollups(idx)
	if err != nil {
		return nil, err
	}

	err = w.indexBundles(idx)
	if err != nil {
		return nil, err
	}

	w.idx = idx

	return idx, nil
}

// indexRollups records each roll-up line's path and byte offset, so a lookup
// re-reads only its one line. A duplicate path keeps the newest line: a
// re-folded member appends an identical record, and a member re-frozen after
// its content changed (a terminal run.json updated between seals) appends a
// newer, different one — the newest is canonical in both cases.
func (w *Workspace) indexRollups(idx map[string]sealedRef) error {
	files, err := listFiles(w.org.AbsPath(path.Join(w.dir, store.RollupsDirName)), ".ndjson")
	if err != nil {
		return err
	}

	for _, file := range files {
		err = indexRollupFile(file, idx)
		if err != nil {
			return err
		}
	}

	return nil
}

// indexRollupFile scans one roll-up, recording each line's path and offset. A
// torn trailing line without its newline commit marker is ignored, matching the
// writer's crash semantics, and a corrupt mid-file line is skipped rather than
// failing the whole index, so one damaged sealed file does not hide the
// workspace's intact loose runs and state versions.
func indexRollupFile(file string, idx map[string]sealedRef) error {
	//nolint:gosec // The path is composed from the archive root being browsed.
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("open roll-up %q: %w", file, err)
	}

	defer func() {
		//nolint:errcheck // Read-only handle.
		_ = f.Close()
	}()

	r := bufio.NewReader(f)

	var offset int64

	for {
		line, readErr := r.ReadBytes('\n')
		if readErr == nil {
			var rec rollupLine

			// A single corrupt line (bit rot, a partial restore) must not dead-end
			// the workspace: skip it and keep indexing. The offset still advances so
			// later lines index at their true byte offset.
			err = json.Unmarshal(line, &rec)
			if err != nil {
				offset += int64(len(line))

				continue
			}

			idx[rec.Path] = sealedRef{rollup: file, offset: offset, size: int64(len(rec.Content))}
			offset += int64(len(line))

			continue
		}

		if errors.Is(readErr, io.EOF) {
			return nil
		}

		return fmt.Errorf("read roll-up %q: %w", file, readErr)
	}
}

// indexBundles records each sidecar entry's member path and bundle, so a lookup
// opens only the one bundle that holds it. Sidecars stay local even when their
// zips are evicted remote, so the index is always built offline.
func (w *Workspace) indexBundles(idx map[string]sealedRef) error {
	relBundles := path.Join(w.dir, store.BundlesDirName)

	files, err := listFiles(w.org.AbsPath(relBundles), ".sidecar.ndjson")
	if err != nil {
		return err
	}

	for _, file := range files {
		err = indexSidecarFile(file, relBundles, idx)
		if err != nil {
			return err
		}
	}

	return nil
}

// indexSidecarFile scans one sidecar index, recording each member against its
// bundle's archive-relative path.
func indexSidecarFile(file, relBundles string, idx map[string]sealedRef) error {
	//nolint:gosec // The path is composed from the archive root being browsed.
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("open sidecar %q: %w", file, err)
	}

	defer func() {
		//nolint:errcheck // Read-only handle.
		_ = f.Close()
	}()

	r := bufio.NewReader(f)

	for {
		line, readErr := r.ReadBytes('\n')
		if readErr == nil {
			var rec sidecarLine

			// Skip a corrupt line rather than dead-ending the workspace screen,
			// matching indexRollupFile and the loose-file browser's per-file
			// resilience. Reading with a bufio.Reader rather than a size-capped
			// bufio.Scanner keeps a merged or oversized garbage run (bit rot, a
			// partial restore) from raising a hard ErrTooLong that would fail the
			// whole index; it simply fails to unmarshal and is skipped.
			err = json.Unmarshal(line, &rec)
			if err == nil {
				idx[rec.Name] = sealedRef{bundle: path.Join(relBundles, rec.Bundle), size: rec.Size}
			}

			continue
		}

		if errors.Is(readErr, io.EOF) {
			return nil
		}

		return fmt.Errorf("read sidecar %q: %w", file, readErr)
	}
}

// readRollupLine re-reads one roll-up line at its recorded offset and returns
// the member's content bytes.
func readRollupLine(file string, offset int64, relPath string) ([]byte, error) {
	//nolint:gosec // The path is composed from the archive root being browsed.
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("open roll-up %q: %w", file, err)
	}

	defer func() {
		//nolint:errcheck // Read-only handle.
		_ = f.Close()
	}()

	_, err = f.Seek(offset, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("seek roll-up %q: %w", file, err)
	}

	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read roll-up %q at offset %d: %w", file, offset, err)
	}

	var rec rollupLine

	err = json.Unmarshal(line, &rec)
	if err != nil {
		return nil, fmt.Errorf("parse roll-up %q at offset %d: %w", file, offset, err)
	}

	if rec.Path != relPath {
		return nil, fmt.Errorf("%w: roll-up line at %q offset %d holds %q", ErrObjectNotFound, file, offset, rec.Path)
	}

	// The sealer records each line's digest over the source bytes; verify the
	// carried content against it, so rot inside the JSON string surfaces as an
	// integrity error rather than silently corrupt content. A line without a
	// digest (a hand-built fixture, an older writer) is served as-is.
	if rec.SHA256 != "" {
		sum := sha256.Sum256([]byte(rec.Content))
		if hex.EncodeToString(sum[:]) != rec.SHA256 {
			return nil, fmt.Errorf("%w: %s in roll-up %q", ErrRollupChecksum, relPath, file)
		}
	}

	return []byte(rec.Content), nil
}

// readLocalBundleMember extracts one member from an on-disk zip bundle by its
// archive-relative path. A missing zip reports [fs.ErrNotExist] through the
// wrap, which is what routes the caller to the bundle's remote copy.
func readLocalBundleMember(bundle, relPath string) ([]byte, error) {
	zr, err := zip.OpenReader(bundle)
	if err != nil {
		return nil, fmt.Errorf("open bundle %q: %w", bundle, err)
	}

	defer func() {
		//nolint:errcheck // Read-only handle.
		_ = zr.Close()
	}()

	for _, f := range zr.File {
		if f.Name != relPath {
			continue
		}

		rc, openErr := f.Open()
		if openErr != nil {
			return nil, fmt.Errorf("open member %q: %w", relPath, openErr)
		}

		// Bound the decompressed member: f.Open inflates on the fly, so an
		// unbounded read of a crafted or corrupt DEFLATE member would OOM the
		// viewer. Read one byte past the cap to catch an oversized member rather
		// than truncate it, matching the remote path (see [maxMemberSize]).
		data, readErr := io.ReadAll(io.LimitReader(rc, maxMemberSize+1))
		closeErr := rc.Close()

		switch {
		case readErr != nil:
			return nil, fmt.Errorf("read member %q: %w", relPath, readErr)
		case closeErr != nil:
			return nil, fmt.Errorf("close member %q: %w", relPath, closeErr)
		case int64(len(data)) > maxMemberSize:
			return nil, fmt.Errorf("member %q exceeds the %d-byte cap", relPath, maxMemberSize)
		}

		return data, nil
	}

	return nil, fmt.Errorf("%w: %s in bundle %q", ErrObjectNotFound, relPath, bundle)
}

// listFiles returns the absolute paths of dir's regular files whose names end
// in suffix, tolerating a dir that does not exist.
func listFiles(dir, suffix string) ([]string, error) {
	entries, err := os.ReadDir(dir)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}

	var files []string

	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}

	return files, nil
}
