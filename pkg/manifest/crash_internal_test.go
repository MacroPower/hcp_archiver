package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/logtest"
)

// forEachCrashPrefix drives one flush through every crash point it can tear at
// and hands each torn-but-recovered ledger to check.
//
// The setup func establishes durable base state (the harness flushes it
// whole), then mutate stages the batch under test. The harness flushes again
// and simulates the crash model the WAL documents: the batch lands in the
// single org-level log as one append that persists as a complete-line prefix,
// so every crash point is a prefix of that append. For each prefix the harness
// clones the archive root, truncates the log accordingly, reloads, and calls
// check with a label naming the crash point.
//
// Invariant checks written against this harness are the referee for the
// ledger's durability-order rules: a guard record violated by any prefix fails
// here mechanically instead of waiting for a power loss to find it.
func forEachCrashPrefix(
	t *testing.T,
	setup func(*Ledger),
	mutate func(*Ledger),
	check func(t *testing.T, label string, torn *Ledger),
) {
	t.Helper()

	root := t.TempDir()

	// The shard subtrees the scenarios record under exist before any record
	// references them, as in production, where an object's file lands before
	// its entry. Without them, every torn reload would discard the scenario's
	// records as an operator deletion and each check would pass vacuously on
	// their absence; the guard logger below fails the run if that ever
	// happens again.
	for _, dir := range []string{
		filepath.Join(root, "projects", "p", "workspaces", "w"),
		filepath.Join(root, configVersionsSegment),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	l, err := Load(root, WithLogger(logtest.FailOn(t, "ledger_records_discarded")))
	require.NoError(t, err)

	if setup != nil {
		setup(l)
		require.NoError(t, l.Flush())
	}

	pre := fileSize(t, l.walPath())

	mutate(l)
	require.NoError(t, l.Flush())

	walPath := l.walPath()
	post := fileSize(t, walPath)

	require.NoError(t, l.Close())
	require.Greater(t, post, pre, "the mutate stage appended nothing; the scenario tests no crash window")

	data, err := os.ReadFile(walPath)
	require.NoError(t, err)

	// Crash offsets within the append: nothing landed, each complete line, and
	// the whole batch (the clean finish).
	offsets := []int64{pre}

	for off := pre; off < post; off++ {
		if data[off] == '\n' {
			offsets = append(offsets, off+1)
		}
	}

	for _, offset := range offsets {
		label := "torn at " + itoa(offset-pre) + "b"
		if offset == post {
			label = "clean finish"
		}

		runTornState(t, root, walPath, offset, label, check)
	}
}

// runTornState clones root, truncates the org log to one crash prefix,
// reloads, and checks.
func runTornState(
	t *testing.T,
	root, walPath string,
	offset int64,
	label string,
	check func(t *testing.T, label string, torn *Ledger),
) {
	t.Helper()

	tornRoot := t.TempDir()
	require.NoError(t, os.CopyFS(tornRoot, os.DirFS(root)))

	rel, err := filepath.Rel(root, walPath)
	require.NoError(t, err)

	require.NoError(t, os.Truncate(filepath.Join(tornRoot, rel), offset))

	// The reload runs under a guard logger: a discard here means the harness
	// state diverged from production (a shard subtree the scenario records
	// under went missing) and every invariant check downstream would pass
	// vacuously on the records' absence.
	l, err := Load(tornRoot, WithLogger(logtest.FailOn(t, "ledger_records_discarded")))
	require.NoError(t, err, "%s: a torn log must load", label)

	defer func() {
		require.NoError(t, l.Close())
	}()

	check(t, label, l)
}

// fileSize returns the size of the file at path, or zero when it does not
// exist yet.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}

	require.NoError(t, err)

	return info.Size()
}

// itoa formats a small offset without pulling strconv into every call site.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}

	var buf [20]byte

	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	return string(buf[i:])
}

// TestCrashPrefix_SettledEntryNeverOutlivesItsGuards pins the entry drain
// order: a settled entry that acts as a skip signal (run-events.json done)
// must never be durable without the unsettled records drained in the same
// batch (the pending actors gate and the errored foreign write), because the
// skip signal alone would stop every future run from retrying the work the
// guards stand for.
func TestCrashPrefix_SettledEntryNeverOutlivesItsGuards(t *testing.T) {
	t.Parallel()

	const (
		events = "projects/p/workspaces/w/runs/r1/run-events.json"
		gate   = "projects/p/workspaces/w/runs/r1/run-events-actors.ref"
		user   = "users/u1.json"
	)

	forEachCrashPrefix(t,
		nil,
		func(l *Ledger) {
			l.RecordErrored(user, errors.New("actor read boom"), true)
			l.MirrorReference(gate, false)
			l.RecordDone(events, Signature{Size: 1})
		},
		func(t *testing.T, label string, torn *Ledger) {
			t.Helper()

			e, ok := torn.Entry(events)
			if !ok || e.Status != StatusDone {
				return // The skip signal is not durable, so nothing is skipped.
			}

			g, ok := torn.Entry(gate)
			require.True(t, ok, "%s: done skip signal durable without its gate", label)
			require.Equal(t, StatusPending, g.Status,
				"%s: the gate must be pending wherever the skip signal is durable", label)

			u, ok := torn.Entry(user)
			require.True(t, ok, "%s: done skip signal durable without the errored foreign write", label)
			require.Equal(t, StatusErrored, u.Status, "%s", label)
		},
	)
}

// TestCrashPrefix_UnsettleGuardLeadsItsEntries pins the withdrawal order: a
// re-walk's freshly frozen entries must never be durable under a stale settled
// flag, or the next run's early stop halts above elements the interrupted walk
// never listed.
func TestCrashPrefix_UnsettleGuardLeadsItsEntries(t *testing.T) {
	t.Parallel()

	const (
		prefix   = "projects/p/workspaces/w/runs"
		oldEntry = prefix + "/r1/run.json"
		newEntry = prefix + "/r2/run.json"
	)

	forEachCrashPrefix(t,
		func(l *Ledger) {
			l.RecordDone(oldEntry, Signature{Size: 1})
			l.Collection(prefix).MarkComplete()
			l.Collection(prefix).SetSettled(true)
		},
		func(l *Ledger) {
			l.Collection(prefix).SetSettled(false)
			l.RecordDone(newEntry, Signature{Size: 1})
		},
		func(t *testing.T, label string, torn *Ledger) {
			t.Helper()

			if _, ok := torn.Entry(newEntry); !ok {
				return // The guarded entry is not durable, so the stale flag is harmless.
			}

			require.False(t, torn.Collection(prefix).Settled(),
				"%s: a frozen entry is durable under a stale settled flag", label)
		},
	)
}

// TestCrashPrefix_SettleTrailsItsEntries pins the earn order: a settled flag
// must never be durable before every entry recorded in the same batch, or the
// early stop would trust a boundary whose elements were lost with the tear.
func TestCrashPrefix_SettleTrailsItsEntries(t *testing.T) {
	t.Parallel()

	const (
		prefix = "projects/p/workspaces/w/runs"
		first  = prefix + "/r1/run.json"
		second = prefix + "/r2/run.json"
	)

	forEachCrashPrefix(t,
		nil,
		func(l *Ledger) {
			l.RecordDone(first, Signature{Size: 1})
			l.RecordDone(second, Signature{Size: 1})
			l.Collection(prefix).MarkComplete()
			l.Collection(prefix).SetSettled(true)
		},
		func(t *testing.T, label string, torn *Ledger) {
			t.Helper()

			if !torn.Collection(prefix).Settled() {
				return
			}

			for _, relPath := range []string{first, second} {
				e, ok := torn.Entry(relPath)
				require.True(t, ok, "%s: settled flag durable without entry %s", label, relPath)
				require.Equal(t, StatusDone, e.Status, "%s", label)
			}
		},
	)
}

// TestCrashPrefix_ReferencedProofPrecedesItsFreeze pins the cross-shard order:
// a run may only be durably frozen behind a settled collection when the
// config-version tarball proof it references is itself durable, or nothing
// would ever re-record the tarball below the early-stop boundary.
func TestCrashPrefix_ReferencedProofPrecedesItsFreeze(t *testing.T) {
	t.Parallel()

	const (
		tarball = configVersionsSegment + "/cv-1/config.tar.gz"
		prefix  = "projects/p/workspaces/w/runs"
		run     = prefix + "/r1/run.json"
	)

	forEachCrashPrefix(t,
		nil,
		func(l *Ledger) {
			l.RecordDone(tarball, Signature{Size: 1})
			l.RecordDone(run, Signature{Size: 1})
			l.Collection(prefix).MarkComplete()
			l.Collection(prefix).SetSettled(true)
		},
		func(t *testing.T, label string, torn *Ledger) {
			t.Helper()

			if label == "clean finish" {
				// The clean finish must carry the whole batch; a scenario whose
				// records vanish on reload would pass every prefix vacuously.
				_, ok := torn.Entry(run)
				require.True(t, ok, "the clean finish lost the scenario's records")
			}

			if _, ok := torn.Entry(run); !ok || !torn.Collection(prefix).Settled() {
				return // The run is not durably frozen, so a lost proof is re-recorded.
			}

			e, ok := torn.Entry(tarball)
			require.True(t, ok, "%s: run frozen behind a settled collection without its tarball proof", label)
			require.Equal(t, StatusDone, e.Status, "%s", label)
		},
	)
}

// TestCrashPrefix_GateClearNeverOutlivesTheHealedEntry pins the settled-gate
// rank: a healing run records the healed foreign entry and the gate clear in
// one batch, and the clear must never be durable without the healed entry: the
// skip conditions gate on the cleared reference, so a durable clear over a
// still-errored entry would disable the retry the gate exists to force,
// permanently.
func TestCrashPrefix_GateClearNeverOutlivesTheHealedEntry(t *testing.T) {
	t.Parallel()

	const (
		user = "users/u1.json"
		gate = "projects/p/workspaces/w/runs/r1/run-events-actors.ref"
	)

	forEachCrashPrefix(t,
		func(l *Ledger) {
			l.RecordErrored(user, errors.New("actor read boom"), true)
			l.MirrorReference(gate, false)
		},
		func(l *Ledger) {
			l.RecordDone(user, Signature{Size: 1})
			l.MirrorReference(gate, true)
		},
		func(t *testing.T, label string, torn *Ledger) {
			t.Helper()

			g, ok := torn.Entry(gate)
			if !ok || g.Status != StatusReferenceCleared {
				return // The clear is not durable, so the retry stays armed.
			}

			u, ok := torn.Entry(user)
			require.True(t, ok, "%s: cleared gate durable without the healed entry", label)
			require.Equal(t, StatusDone, u.Status,
				"%s: the healed entry must be durable wherever its clear is", label)
		},
	)
}
