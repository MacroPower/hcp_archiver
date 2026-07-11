package view

import (
	"archive/zip"
	"bufio"
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
)

// ErrObjectNotFound indicates an archive-relative path is present in none of
// the archive's physical forms.
var ErrObjectNotFound = errors.New("object not found in archive")

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
// carries its bytes.
type sealedRef struct {
	rollup string
	bundle string
	offset int64
}

// rollupLine mirrors one roll-up record: the member's archive-relative path and
// its content carried verbatim as a JSON string.
type rollupLine struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// sidecarLine mirrors one bundle sidecar record: the member's archive-relative
// path and the bundle that holds it.
type sidecarLine struct {
	Name   string `json:"name"`
	Bundle string `json:"bundle"`
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

	return readBundleMember(ref.bundle, relPath)
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
// objects that sit under an archive-relative directory prefix.
func (w *Workspace) sealedNames(dirPrefix string) ([]string, error) {
	idx, err := w.index()
	if err != nil {
		return nil, err
	}

	prefix := dirPrefix + "/"

	var names []string

	for key := range idx {
		if strings.HasPrefix(key, prefix) {
			names = append(names, key)
		}
	}

	return names, nil
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
// re-reads only its one line. A duplicate path keeps the newest line, matching
// how a re-folded member appends an identical record.
func (w *Workspace) indexRollups(idx map[string]sealedRef) error {
	files, err := listFiles(w.org.AbsPath(path.Join(w.dir, "rollups")), ".ndjson")
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
// writer's crash semantics.
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

			err = json.Unmarshal(line, &rec)
			if err != nil {
				return fmt.Errorf("parse roll-up %q at offset %d: %w", file, offset, err)
			}

			idx[rec.Path] = sealedRef{rollup: file, offset: offset}
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
// opens only the one bundle that holds it.
func (w *Workspace) indexBundles(idx map[string]sealedRef) error {
	bundlesDir := w.org.AbsPath(path.Join(w.dir, "bundles"))

	files, err := listFiles(bundlesDir, ".sidecar.ndjson")
	if err != nil {
		return err
	}

	for _, file := range files {
		err = indexSidecarFile(file, bundlesDir, idx)
		if err != nil {
			return err
		}
	}

	return nil
}

// indexSidecarFile scans one sidecar index, recording each member against its
// bundle's absolute path.
func indexSidecarFile(file, bundlesDir string, idx map[string]sealedRef) error {
	//nolint:gosec // The path is composed from the archive root being browsed.
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("open sidecar %q: %w", file, err)
	}

	defer func() {
		//nolint:errcheck // Read-only handle.
		_ = f.Close()
	}()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(nil, 1<<20)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var rec sidecarLine

		err = json.Unmarshal(line, &rec)
		if err != nil {
			return fmt.Errorf("parse sidecar %q: %w", file, err)
		}

		idx[rec.Name] = sealedRef{bundle: filepath.Join(bundlesDir, rec.Bundle)}
	}

	err = scanner.Err()
	if err != nil {
		return fmt.Errorf("read sidecar %q: %w", file, err)
	}

	return nil
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

	return []byte(rec.Content), nil
}

// readBundleMember extracts one member from a zip bundle by its
// archive-relative path.
func readBundleMember(bundle, relPath string) ([]byte, error) {
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

		data, readErr := io.ReadAll(rc)
		closeErr := rc.Close()

		switch {
		case readErr != nil:
			return nil, fmt.Errorf("read member %q: %w", relPath, readErr)
		case closeErr != nil:
			return nil, fmt.Errorf("close member %q: %w", relPath, closeErr)
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
