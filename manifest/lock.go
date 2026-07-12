package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockFileName is the flock target inside the org-root ledger directory. The
// file's presence on disk carries no meaning; only the kernel lock on it
// does, so a file left behind by a crashed process is inert.
const lockFileName = "lock"

// ErrLedgerLocked reports that another live process holds the ledger's
// exclusive lock. The manifest's write-ahead log, compaction, and in-memory
// tallies all assume a single writer, so a second archiver on the same root
// (typically a scheduled run starting while the previous one still runs) must
// fail fast rather than silently interleave.
var ErrLedgerLocked = errors.New("ledger is locked by another process")

// acquireLock takes an exclusive, non-blocking flock on the lock file under
// dir, creating both as needed, and returns the held file.
//
// An flock is the mechanism precisely because of what happens on a crash: the
// kernel releases the lock the moment the holding process dies (its file
// descriptors close), so a killed or panicked run can never leave a stale
// lock behind, and no PID file, liveness probe, or manual recovery exists
// because none is needed. The trade-off is that flock is advisory and weakly
// supported on some network filesystems (old NFS), where the exclusion is
// best-effort.
func acquireLock(dir string) (*os.File, error) {
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return nil, fmt.Errorf("create ledger dir %q: %w", dir, err)
	}

	path := filepath.Join(dir, lockFileName)

	//nolint:gosec // The lock path is derived from the operator-chosen root.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open ledger lock %q: %w", path, err)
	}

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		_ = f.Close() //nolint:errcheck // The lock was never acquired.

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %q", ErrLedgerLocked, path)
		}

		return nil, fmt.Errorf("lock ledger %q: %w", path, err)
	}

	return f, nil
}
