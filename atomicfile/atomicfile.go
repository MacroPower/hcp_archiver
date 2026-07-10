package atomicfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Default permissions applied when an [Option] leaves a mode unset. They are
// owner-only: this package backs an archive whose raw state blobs hold secrets
// in cleartext, so a written file and any directory created for it are readable
// only by the user that wrote them rather than world-readable.
const (
	// DefaultFileMode is the mode of a written file when none is given.
	DefaultFileMode fs.FileMode = 0o600
	// DefaultDirMode is the mode of a created parent directory when none is
	// given.
	DefaultDirMode fs.FileMode = 0o700
)

// tmpPattern is the [os.CreateTemp] pattern for the staging file. The leading
// dot keeps the transient file out of ordinary directory listings, and the
// suffix marks it as a partial write that a crash may leave behind.
const tmpPattern = ".atomicfile-*.tmp"

// config holds the resolved settings for a single write.
type config struct {
	fileMode fs.FileMode
	dirMode  fs.FileMode
}

// Option configures an atomic write.
//
// The available options are:
//   - [WithFileMode]
//   - [WithDirMode]
type Option func(*config)

// WithFileMode sets the mode applied to the written file, overriding
// [DefaultFileMode]. It returns an [Option].
func WithFileMode(mode fs.FileMode) Option {
	return func(c *config) {
		c.fileMode = mode
	}
}

// WithDirMode sets the mode applied to any parent directory created for the
// write, overriding [DefaultDirMode]. It returns an [Option].
func WithDirMode(mode fs.FileMode) Option {
	return func(c *config) {
		c.dirMode = mode
	}
}

// WriteFile atomically writes data to name.
//
// It is the byte-slice form of [Write]; see that function for the durability
// guarantee and the treatment of parent directories.
func WriteFile(name string, data []byte, opts ...Option) error {
	return Write(name, func(w io.Writer) error {
		_, err := w.Write(data)
		if err != nil {
			return fmt.Errorf("write bytes: %w", err)
		}

		return nil
	}, opts...)
}

// WriteReader atomically writes the full contents of r to name.
//
// It is the streaming form of [Write] and copies r straight to the staging
// file without buffering the payload, so an arbitrarily large state blob or
// configuration tarball streams through in bounded memory. See [Write] for the
// durability guarantee and the treatment of parent directories.
func WriteReader(name string, r io.Reader, opts ...Option) error {
	return Write(name, func(w io.Writer) error {
		_, err := io.Copy(w, r)
		if err != nil {
			return fmt.Errorf("copy contents: %w", err)
		}

		return nil
	}, opts...)
}

// Write atomically writes to name whatever fn emits to the [io.Writer] it is
// handed.
//
// It is the primitive that [WriteFile] and [WriteReader] build on. The contents
// are staged in a temporary file in name's own directory, flushed to stable
// storage, and renamed into place; because the rename is atomic on POSIX, a
// reader never observes a half-written file and an interrupted run never leaves
// a truncated one that looks complete. After the rename the parent directory is
// itself flushed so the rename survives a crash.
//
// Any missing parent directory of name is created with the directory mode (see
// [WithDirMode]), and every directory the write creates is flushed to stable
// storage, so a first write into a fresh subtree (and the .ledger subtree it may
// bring into being) is durable, not just its leaf. The written file takes the
// file mode (see [WithFileMode]).
//
// When fn returns an error, or any step of the write fails, the staging file is
// removed and name is left exactly as it was, whether pre-existing or absent.
func Write(name string, fn func(io.Writer) error, opts ...Option) error {
	cfg := config{
		fileMode: DefaultFileMode,
		dirMode:  DefaultDirMode,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	dir := filepath.Dir(name)

	err := mkdirAllSynced(dir, cfg.dirMode)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return fmt.Errorf("create staging file in %q: %w", dir, err)
	}

	tmpName := tmp.Name()

	err = stage(tmp, tmpName, name, cfg.fileMode, fn)
	if err != nil {
		//nolint:gosec // Best-effort cleanup of the staging file on the error path.
		os.Remove(tmpName)

		return err
	}

	err = syncDir(dir)
	if err != nil {
		return fmt.Errorf("sync parent directory %q: %w", dir, err)
	}

	return nil
}

// stage writes fn's output into the already-open staging file, gives it mode,
// flushes and closes it, and renames it onto name. The file is always closed
// before stage returns, even on error, so the caller only has to remove the
// staging path.
func stage(f *os.File, tmpName, name string, mode fs.FileMode, fn func(io.Writer) error) error {
	opErr := fn(f)
	if opErr == nil {
		opErr = f.Chmod(mode)
	}

	if opErr == nil {
		opErr = f.Sync()
	}

	closeErr := f.Close()

	switch {
	case opErr != nil:
		return fmt.Errorf("write staging file: %w", opErr)
	case closeErr != nil:
		return fmt.Errorf("close staging file: %w", closeErr)
	}

	err := os.Rename(tmpName, name)
	if err != nil {
		return fmt.Errorf("rename staging file into place: %w", err)
	}

	return nil
}

// Append appends data to the append-only file at name and returns the offset the
// data was written at.
//
// It is the durable, append-only counterpart to [Write] for newline-delimited
// logs and roll-ups. A newline terminates each record and is its commit marker,
// so a crash mid-append can leave an uncommitted fragment past the final
// newline. Append trims that fragment before writing, landing the new data on a
// record boundary rather than fusing it onto a half-written line; a wholly
// unterminated file is trimmed to empty. The batch is flushed to stable storage,
// and the parent directory is synced too so a first append that creates the file
// survives a crash. Callers pass newline-terminated data.
//
// Any missing parent directory of name is created with the directory mode (see
// [WithDirMode]), and every directory created for it is flushed, so a first
// append into a fresh subtree is durable; a created file takes the file mode
// (see [WithFileMode]).
func Append(name string, data []byte, opts ...Option) (int64, error) {
	cfg := config{
		fileMode: DefaultFileMode,
		dirMode:  DefaultDirMode,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	dir := filepath.Dir(name)

	err := mkdirAllSynced(dir, cfg.dirMode)
	if err != nil {
		return 0, err
	}

	//nolint:gosec // The append target is chosen by the caller by design.
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, cfg.fileMode)
	if err != nil {
		return 0, fmt.Errorf("open append target %q: %w", name, err)
	}

	start, err := appendCommitted(f, data)
	if err != nil {
		return 0, err
	}

	err = syncDir(dir)
	if err != nil {
		return 0, fmt.Errorf("sync parent directory %q: %w", dir, err)
	}

	return start, nil
}

// appendCommitted trims any torn trailing fragment from f, writes data at the
// resulting record boundary, and flushes it, returning the offset the write
// began at. It closes f on every path, mirroring [stage].
func appendCommitted(f *os.File, data []byte) (int64, error) {
	start, opErr := lastRecordBoundary(f)
	if opErr == nil {
		opErr = f.Truncate(start)
	}

	if opErr == nil {
		_, opErr = f.WriteAt(data, start)
	}

	if opErr == nil {
		opErr = f.Sync()
	}

	closeErr := f.Close()

	switch {
	case opErr != nil:
		return 0, fmt.Errorf("append at record boundary: %w", opErr)
	case closeErr != nil:
		return 0, fmt.Errorf("close append target: %w", closeErr)
	}

	return start, nil
}

// lastRecordBoundary returns the offset just past f's final newline, the end of
// its last committed record. It scans backward from the end so a large log costs
// one read rather than a whole-file scan; a file with no newline yields 0.
func lastRecordBoundary(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat append target: %w", err)
	}

	const chunk = 4096

	buf := make([]byte, chunk)

	for off := info.Size(); off > 0; {
		n := min(int64(chunk), off)
		off -= n

		_, err = f.ReadAt(buf[:n], off)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("scan for record boundary: %w", err)
		}

		idx := bytes.LastIndexByte(buf[:n], '\n')
		if idx >= 0 {
			return off + int64(idx) + 1, nil
		}
	}

	return 0, nil
}

// mkdirAllSynced creates dir and any missing ancestor, then flushes each newly
// created directory's parent so the dentries linking the new subtree are
// durable, not just the leaf's.
func mkdirAllSynced(dir string, mode fs.FileMode) error {
	return mkdirAllSync(dir, mode, syncDir)
}

// mkdirAllSync is [mkdirAllSynced] with the directory-sync function injected, so
// a test can record which directories are flushed.
//
// It fast-paths an already-existing directory with a single Stat and no fsync,
// the hot path taken by every write after the first into a directory. Otherwise
// it scans upward from dir collecting the levels that do not yet exist — exactly
// the ones [os.MkdirAll] creates — creates them in one MkdirAll call (keeping its
// race, mode, and ENOTDIR handling), then flushes the parent of every scanned
// level. The parents are flushed unconditionally, even one a concurrent creator
// won the race to make, because the dentry still needs flushing from this
// process and a redundant fsync is harmless.
func mkdirAllSync(dir string, mode fs.FileMode, sync func(string) error) error {
	info, err := os.Stat(dir)
	if err == nil && info.IsDir() {
		return nil
	}

	var missing []string

	for level := dir; ; {
		_, statErr := os.Stat(level)
		if statErr == nil {
			break
		}

		if !errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("stat directory %q: %w", level, statErr)
		}

		missing = append(missing, level)

		parent := filepath.Dir(level)
		if parent == level {
			break
		}

		level = parent
	}

	err = os.MkdirAll(dir, mode)
	if err != nil {
		return fmt.Errorf("create parent directory %q: %w", dir, err)
	}

	for _, level := range missing {
		parent := filepath.Dir(level)

		err = sync(parent)
		if err != nil {
			return fmt.Errorf("sync created directory %q: %w", parent, err)
		}
	}

	return nil
}

// syncDir flushes dir so a rename recorded within it is durable across a crash.
func syncDir(dir string) error {
	//nolint:gosec // The directory to fsync is chosen by the caller by design.
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}

	syncErr := d.Sync()
	closeErr := d.Close()

	switch {
	case syncErr != nil:
		return fmt.Errorf("sync: %w", syncErr)
	case closeErr != nil:
		return fmt.Errorf("close: %w", closeErr)
	}

	return nil
}
