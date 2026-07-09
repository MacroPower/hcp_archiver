package atomicfile

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Default permissions applied when an [Option] leaves a mode unset.
const (
	// DefaultFileMode is the mode of a written file when none is given.
	DefaultFileMode fs.FileMode = 0o644
	// DefaultDirMode is the mode of a created parent directory when none is
	// given.
	DefaultDirMode fs.FileMode = 0o755
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
// [WithDirMode]). The written file takes the file mode (see [WithFileMode]).
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

	err := os.MkdirAll(dir, cfg.dirMode)
	if err != nil {
		return fmt.Errorf("create parent directory %q: %w", dir, err)
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
