package manifest_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/manifest"
)

// fixedClock returns a clock function yielding a constant time.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time {
		return t
	}
}

// rootShardFiles returns the org-root shard's snapshot and log paths under an
// archive root; objects with no workspace or stack prefix key to that shard.
func rootShardFiles(root string) (string, string) {
	dir := filepath.Join(root, ".ledger")

	return filepath.Join(dir, "snapshot.json"), filepath.Join(dir, "log.ndjson")
}

func TestStatus_String(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "done", manifest.StatusDone.String())
	assert.Equal(t, "absent", manifest.StatusAbsent.String())
	assert.Equal(t, "forbidden", manifest.StatusForbidden.String())
	assert.Equal(t, "not-applicable", manifest.StatusNotApplicable.String())
}

func TestStatus_ValidAndSettled(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status      manifest.Status
		wantValid   bool
		wantSettled bool
		wantGate    bool
	}{
		"done":      {status: manifest.StatusDone, wantValid: true, wantSettled: true},
		"absent":    {status: manifest.StatusAbsent, wantValid: true, wantSettled: true},
		"skipped":   {status: manifest.StatusSkipped, wantValid: true, wantSettled: true},
		"na":        {status: manifest.StatusNotApplicable, wantValid: true, wantSettled: true},
		"errored":   {status: manifest.StatusErrored, wantValid: true, wantSettled: false},
		"forbidden": {status: manifest.StatusForbidden, wantValid: true, wantSettled: false},
		"pending":   {status: manifest.StatusPending, wantValid: true, wantSettled: false, wantGate: true},
		"cleared":   {status: manifest.StatusReferenceCleared, wantValid: true, wantSettled: true, wantGate: true},
		"unknown":   {status: manifest.Status("nonsense"), wantValid: false, wantSettled: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.wantValid, tc.status.Valid())
			assert.Equal(t, tc.wantSettled, tc.status.Settled())
			assert.Equal(t, tc.wantGate, tc.status.IsGate())
		})
	}
}

func TestSignatureOf_Equal(t *testing.T) {
	t.Parallel()

	a := manifest.SignatureOf([]byte("hello"))
	b := manifest.SignatureOf([]byte("hello"))
	c := manifest.SignatureOf([]byte("world!"))

	assert.Equal(t, int64(5), a.Size)
	assert.NotEmpty(t, a.Hash)
	assert.True(t, a.Equal(b))
	assert.False(t, a.Equal(c))

	// Size-only fallback when a hash is missing.
	sizeOnly := manifest.Signature{Size: 5}
	assert.True(t, sizeOnly.Equal(manifest.Signature{Size: 5}))
	assert.False(t, sizeOnly.Equal(manifest.Signature{Size: 6}))
}

func TestLoad_EmptyWhenMissing(t *testing.T) {
	t.Parallel()

	path := t.TempDir()

	ledger, err := manifest.Load(path)
	require.NoError(t, err)

	_, ok := ledger.Entry("anything")
	assert.False(t, ok)
	assert.Equal(t, 0, ledger.RunCount())
	assert.True(t, ledger.ShouldFetch("anything"))
}

func TestLoad_Corrupt(t *testing.T) {
	t.Parallel()

	path := t.TempDir()
	snapshot, _ := rootShardFiles(path)
	require.NoError(t, os.MkdirAll(filepath.Dir(snapshot), 0o755))
	require.NoError(t, os.WriteFile(snapshot, []byte("{ not json"), 0o600))

	_, err := manifest.Load(path)
	require.ErrorIs(t, err, manifest.ErrCorruptManifest)
}

func TestLoad_UnknownStatus(t *testing.T) {
	t.Parallel()

	// A status outside the recognized set would seed the cumulative tally under a
	// key the tally never reads, so a resumed run would report a zero total; the
	// load rejects it instead.
	path := t.TempDir()
	doc := `{"version":1,"entries":{"a":{"status":"nonsense","attempts":1}}}`
	snapshot, _ := rootShardFiles(path)
	require.NoError(t, os.MkdirAll(filepath.Dir(snapshot), 0o755))
	require.NoError(t, os.WriteFile(snapshot, []byte(doc), 0o600))

	_, err := manifest.Load(path)
	require.ErrorIs(t, err, manifest.ErrCorruptManifest)
}

func TestLedger_RoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 8, 12, 0, 0, 0, time.UTC)
	path := t.TempDir()

	ledger, err := manifest.Load(path, manifest.WithClock(fixedClock(now)))
	require.NoError(t, err)

	ledger.StartRun()

	sig := manifest.SignatureOf([]byte("state blob"))
	ledger.RecordDone("ws/state.json", sig)
	ledger.RecordAbsent("ws/expired.tar.gz", errors.New("resource not found"))
	ledger.RecordErrored("ws/run.log", errors.New("boom"), true)
	ledger.RecordForbidden("github-app-installations.json", errors.New("forbidden: team tokens not supported"))
	ledger.RecordSkipped("ws/deferred.json")
	ledger.RecordNotApplicable("ws/ssh-keys.json")
	ledger.Collection("state-versions").AdvanceHighWaterMark(now)
	ledger.AddBytes(1024)

	summary := ledger.FinishRun()
	assert.Equal(t, 1, summary.Totals[manifest.StatusDone])
	assert.Equal(t, int64(1024), summary.BytesDownloaded)

	require.NoError(t, ledger.Flush())

	// Reload and confirm the durable state survives.
	require.NoError(t, ledger.Close(), "release the lock as a finished process would")

	reloaded, err := manifest.Load(path, manifest.WithClock(fixedClock(now)))
	require.NoError(t, err)

	assert.Equal(t, 1, reloaded.RunCount())
	assert.Equal(t, now, reloaded.LastRunAt())
	assert.Equal(t, now, reloaded.Collection("state-versions").HighWaterMark())

	done, ok := reloaded.Entry("ws/state.json")
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, done.Status)
	assert.Equal(t, now, done.FirstSeen)
	assert.Equal(t, now, done.FetchedAt)
	require.NotNil(t, done.Signature)
	assert.True(t, done.Signature.Equal(sig))

	errd, ok := reloaded.Entry("ws/run.log")
	require.True(t, ok)
	assert.Equal(t, manifest.StatusErrored, errd.Status)
	assert.Equal(t, "boom", errd.LastError)
	assert.True(t, errd.Transient)

	// A forbidden entry survives reload keeping its cause, and stays retryable so
	// a re-run under a broader token captures a superset.
	forb, ok := reloaded.Entry("github-app-installations.json")
	require.True(t, ok)
	assert.Equal(t, manifest.StatusForbidden, forb.Status)
	assert.Equal(t, "forbidden: team tokens not supported", forb.LastError)
	assert.False(t, forb.Transient)
	assert.True(t, reloaded.ShouldFetch("github-app-installations.json"))

	last, ok := reloaded.LastRun()
	require.True(t, ok)
	assert.Equal(t, 1, last.Totals[manifest.StatusDone])
	assert.Equal(t, int64(1024), last.BytesDownloaded)
}

func TestLedger_ShouldFetch(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		record          func(l *manifest.Ledger, path string)
		wantPlain       bool
		wantRetryAbsent bool
	}{
		"done is settled": {
			record:          func(l *manifest.Ledger, p string) { l.RecordDone(p, manifest.Signature{Size: 1}) },
			wantPlain:       false,
			wantRetryAbsent: false,
		},
		"skipped is settled": {
			record:          func(l *manifest.Ledger, p string) { l.RecordSkipped(p) },
			wantPlain:       false,
			wantRetryAbsent: false,
		},
		"not-applicable is settled": {
			record:          func(l *manifest.Ledger, p string) { l.RecordNotApplicable(p) },
			wantPlain:       false,
			wantRetryAbsent: false,
		},
		"errored is retried": {
			record:          func(l *manifest.Ledger, p string) { l.RecordErrored(p, errors.New("x"), false) },
			wantPlain:       true,
			wantRetryAbsent: true,
		},
		"forbidden is retried": {
			record:          func(l *manifest.Ledger, p string) { l.RecordForbidden(p, errors.New("forbidden")) },
			wantPlain:       true,
			wantRetryAbsent: true,
		},
		"absent is sticky but retryable": {
			record:          func(l *manifest.Ledger, p string) { l.RecordAbsent(p, errors.New("resource not found")) },
			wantPlain:       false,
			wantRetryAbsent: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			plain, err := manifest.Load(t.TempDir())
			require.NoError(t, err)
			tc.record(plain, "obj")
			assert.Equal(t, tc.wantPlain, plain.ShouldFetch("obj"))

			retry, err := manifest.Load(
				t.TempDir(),
				manifest.WithRetryAbsent(true),
			)
			require.NoError(t, err)
			tc.record(retry, "obj")
			assert.Equal(t, tc.wantRetryAbsent, retry.ShouldFetch("obj"))
		})
	}
}

func TestLedger_ShouldFetch_Unknown(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	assert.True(t, ledger.ShouldFetch("never-seen"))
}

func TestLedger_HighWaterMarkMonotonic(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	early := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)

	ledger.Collection("runs").AdvanceHighWaterMark(late)
	assert.Equal(t, late, ledger.Collection("runs").HighWaterMark())

	// An earlier value never moves the mark backward.
	ledger.Collection("runs").AdvanceHighWaterMark(early)
	assert.Equal(t, late, ledger.Collection("runs").HighWaterMark())

	assert.True(t, ledger.Collection("unset").HighWaterMark().IsZero())
}

func TestLedger_TallyMatchesRecords(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordDone("a", manifest.Signature{Size: 1})
	ledger.RecordDone("b", manifest.Signature{Size: 1})
	ledger.RecordErrored("c", errors.New("x"), false)
	ledger.RecordSkipped("d")
	ledger.RecordAbsent("e", errors.New("resource not found"))
	ledger.RecordNotApplicable("f")
	ledger.RecordForbidden("g", errors.New("forbidden"))
	ledger.AddBytes(42)
	ledger.AddRetry()
	ledger.AddRetry()

	tally := ledger.Tally()
	assert.Equal(t, 2, tally.Done)
	assert.Equal(t, 1, tally.Errored)
	assert.Equal(t, 1, tally.Forbidden)
	assert.Equal(t, 1, tally.Skipped)
	assert.Equal(t, 1, tally.Absent)
	assert.Equal(t, 1, tally.NotApplicable)
	assert.Equal(t, int64(42), tally.BytesDownloaded)
	assert.Equal(t, int64(2), tally.Retried)
	assert.Equal(t, 7, tally.Total(), "retries are activity, not objects, so they never inflate the total")

	// Retries are a per-run activity figure like BytesDownloaded: a new run
	// starts the count over rather than carrying prior-run re-work forward.
	ledger.StartRun()
	assert.Equal(t, int64(0), ledger.Tally().Retried)
}

func TestLedger_Failures(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordDone("a", manifest.Signature{Size: 1})
	ledger.RecordForbidden("z-forbidden", errors.New("access denied"))
	ledger.RecordErrored("m-errored", errors.New("boom"), true)
	ledger.RecordErrored("b-errored", errors.New("kaput"), false)
	// An object that errored and later succeeded is not a failure.
	ledger.RecordErrored("c", errors.New("transient"), true)
	ledger.RecordDone("c", manifest.Signature{Size: 1})

	want := []manifest.Failure{
		{RelPath: "b-errored", Error: "kaput", Status: manifest.StatusErrored},
		{RelPath: "m-errored", Error: "boom", Status: manifest.StatusErrored},
		{RelPath: "z-forbidden", Error: "access denied", Status: manifest.StatusForbidden},
	}

	assert.Equal(t, want, ledger.Failures(), "errored first, sorted by path within a status")
}

func TestLedger_CumulativeAcrossRuns(t *testing.T) {
	t.Parallel()

	path := t.TempDir()

	// First run archives two objects and errors a third, then persists.
	first, err := manifest.Load(path)
	require.NoError(t, err)

	first.StartRun()
	assert.False(t, first.Tally().Resumed, "a first run is not a resume")

	first.RecordDone("a", manifest.Signature{Size: 1})
	first.RecordDone("b", manifest.Signature{Size: 1})
	first.RecordErrored("c", errors.New("x"), true)
	require.NoError(t, first.Flush())

	// Second run resumes: the cumulative counts carry the prior settled work
	// before this run records anything, and the run is marked resumed.
	require.NoError(t, first.Close(), "release the lock as a finished process would")

	second, err := manifest.Load(path)
	require.NoError(t, err)

	second.StartRun()

	resumed := second.Tally()
	assert.True(t, resumed.Resumed, "a run over an existing manifest resumes")
	assert.Equal(t, 2, resumed.Done, "prior done carries over before any record")
	assert.Equal(t, 1, resumed.Errored)
	assert.Equal(t, 3, resumed.Total())

	// Retrying the errored object moves it to done in the cumulative counts.
	second.RecordDone("c", manifest.Signature{Size: 1})

	after := second.Tally()
	assert.Equal(t, 3, after.Done)
	assert.Equal(t, 0, after.Errored)

	// The per-run record still reflects only this run's work, not the carryover.
	summary := second.FinishRun()
	assert.Equal(t, 1, summary.Totals[manifest.StatusDone], "the run record stays per-run")
}

func TestLedger_CumulativeReRecordIsIdempotent(t *testing.T) {
	t.Parallel()

	path := t.TempDir()

	first, err := manifest.Load(path)
	require.NoError(t, err)

	first.StartRun()
	first.RecordDone("m", manifest.Signature{Size: 1})
	// Re-recording the same status within a run must not inflate the count.
	first.RecordDone("m", manifest.Signature{Size: 2})
	assert.Equal(t, 1, first.Tally().Done, "re-record keeps the object counted once")
	require.NoError(t, first.Flush())

	// A resumed run that re-reads the same mutable object (RecordDone again)
	// must keep the cumulative count at one, not two.
	require.NoError(t, first.Close(), "release the lock as a finished process would")

	second, err := manifest.Load(path)
	require.NoError(t, err)

	second.StartRun()
	assert.Equal(t, 1, second.Tally().Done, "carried over once")

	second.RecordDone("m", manifest.Signature{Size: 1})
	assert.Equal(t, 1, second.Tally().Done, "re-read on resume does not double-count")
}

func TestLedger_ReRecordSwapsTally(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordErrored("obj", errors.New("transient"), true)
	assert.Equal(t, 1, ledger.Tally().Errored)

	// A retry that succeeds must move the object out of the errored count.
	ledger.RecordDone("obj", manifest.Signature{Size: 1})

	tally := ledger.Tally()
	assert.Equal(t, 0, tally.Errored)
	assert.Equal(t, 1, tally.Done)
	assert.Equal(t, 1, tally.Total())
}

func TestLedger_TransientVsTerminal(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.RecordErrored("transient", errors.New("429"), true)
	ledger.RecordErrored("terminal", errors.New("500-repeated"), false)

	tr, ok := ledger.Entry("transient")
	require.True(t, ok)
	assert.True(t, tr.Transient)

	te, ok := ledger.Entry("terminal")
	require.True(t, ok)
	assert.False(t, te.Transient)
}

func TestLedger_AttemptsAccumulate(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.RecordErrored("obj", errors.New("first"), true)
	ledger.RecordErrored("obj", errors.New("second"), true)
	ledger.RecordDone("obj", manifest.Signature{Size: 1})

	e, ok := ledger.Entry("obj")
	require.True(t, ok)
	assert.Equal(t, 3, e.Attempts)
	assert.Equal(t, manifest.StatusDone, e.Status)
}

func TestLedger_EntryCopyIsIsolated(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.RecordDone("obj", manifest.SignatureOf([]byte("payload")))

	first, ok := ledger.Entry("obj")
	require.True(t, ok)
	require.NotNil(t, first.Signature)

	// Mutating the returned copy must not affect the ledger's record.
	first.Signature.Size = -1

	second, ok := ledger.Entry("obj")
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, second.Status)
	assert.Equal(t, int64(7), second.Signature.Size)
}

func TestLedger_ConcurrentRecords(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.StartRun()

	const workers = 16

	const perWorker = 50

	var wg sync.WaitGroup

	wg.Add(workers)

	for w := range workers {
		go func() {
			defer wg.Done()

			for i := range perWorker {
				path := filepath.Join("ws", string(rune('a'+w)), string(rune('0'+i%10)))
				ledger.RecordDone(path, manifest.Signature{Size: int64(i)})
				ledger.AddBytes(1)
				ledger.Collection("runs").AdvanceHighWaterMark(time.Unix(int64(i), 0))

				_ = ledger.Tally()
				_ = ledger.ShouldFetch(path)
			}
		}()
	}

	wg.Wait()

	require.NoError(t, ledger.Flush())
	assert.Equal(t, int64(workers*perWorker), ledger.Tally().BytesDownloaded)
}

func TestLedger_AppendOnlyFlushLeavesLog(t *testing.T) {
	t.Parallel()

	path := t.TempDir()

	ledger, err := manifest.Load(path)
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordDone("ws/a.json", manifest.Signature{Size: 3})
	ledger.RecordErrored("ws/b.log", errors.New("boom"), true)
	require.NoError(t, ledger.Flush())

	// A flush mid-run only appends: the log holds the delta and no compacted
	// snapshot has been written yet.
	snapshot, logFile := rootShardFiles(path)

	_, statErr := os.Stat(snapshot)
	assert.True(t, os.IsNotExist(statErr), "no snapshot before compaction")

	_, statErr = os.Stat(logFile)
	require.NoError(t, statErr, "the append-only log holds the flushed records")

	// The records survive a reload driven by replaying the log alone.
	require.NoError(t, ledger.Close(), "release the lock as a finished process would")

	reloaded, err := manifest.Load(path)
	require.NoError(t, err)

	done, ok := reloaded.Entry("ws/a.json")
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, done.Status)

	errd, ok := reloaded.Entry("ws/b.log")
	require.True(t, ok)
	assert.Equal(t, manifest.StatusErrored, errd.Status)
	assert.True(t, errd.Transient)
}

func TestLedger_FlushFailureKeepsStateForRetry(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root defeats the read-only directory that forces the append failure")
	}

	path := t.TempDir()

	// The workspace subtree exists before records reference it, as in a real
	// run where the object files land first; a reload discards log records for
	// a subtree that is gone (see TestLoad_DeletedSubtreeDiscardsItsLogRecords).
	require.NoError(t, os.MkdirAll(filepath.Join(path, "projects", "p", "workspaces", "w"), 0o755))

	ledger, err := manifest.Load(path)
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordDone("projects/p/workspaces/w/runs/r/run.json", manifest.Signature{Size: 1})
	ledger.RecordDone("org.json", manifest.Signature{Size: 2})

	// A read-only ledger directory makes the org log's append fail; the delta a
	// flush drained up front must be restored rather than dropped.
	ledgerDir := filepath.Join(path, manifest.LedgerDirName)
	require.NoError(t, os.Chmod(ledgerDir, 0o500))
	require.Error(t, ledger.Flush(), "an append into a read-only ledger directory must fail")

	// A retry after the write barrier lifts must re-append the same delta, so
	// the records the failed flush drained are still there to persist.
	require.NoError(t, os.Chmod(ledgerDir, 0o755))
	require.NoError(t, ledger.Flush())

	require.NoError(t, ledger.Close(), "release the lock as a finished process would")

	reloaded, err := manifest.Load(path)
	require.NoError(t, err)

	for _, relPath := range []string{"projects/p/workspaces/w/runs/r/run.json", "org.json"} {
		entry, ok := reloaded.Entry(relPath)
		require.True(t, ok, "entry %q survives the failed-then-retried flush", relPath)
		assert.Equal(t, manifest.StatusDone, entry.Status)
	}
}

func TestLedger_CompactionOnFinishClearsLog(t *testing.T) {
	t.Parallel()

	path := t.TempDir()

	ledger, err := manifest.Load(path)
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordDone("ws/a.json", manifest.Signature{Size: 3})
	ledger.FinishRun()
	require.NoError(t, ledger.Flush())

	// Finishing a run folds the log into the snapshot: a completed archive leaves
	// a current manifest and no leftover log.
	snapshot, logFile := rootShardFiles(path)

	_, statErr := os.Stat(snapshot)
	require.NoError(t, statErr, "the snapshot is written on a finished-run flush")

	_, statErr = os.Stat(logFile)
	assert.True(t, os.IsNotExist(statErr), "the log is removed after compaction")

	require.NoError(t, ledger.Close(), "release the lock as a finished process would")

	reloaded, err := manifest.Load(path)
	require.NoError(t, err)

	done, ok := reloaded.Entry("ws/a.json")
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, done.Status)
	assert.Equal(t, 1, reloaded.RunCount())
}

func TestLedger_SnapshotPlusLogMerge(t *testing.T) {
	t.Parallel()

	path := t.TempDir()

	// First run finishes, compacting a snapshot for object a.
	first, err := manifest.Load(path)
	require.NoError(t, err)

	first.StartRun()
	first.RecordDone("a", manifest.Signature{Size: 1})
	first.FinishRun()
	require.NoError(t, first.Flush())

	// Second run appends object b to the log without compacting, so the durable
	// state is the snapshot (a) plus the log (b).
	require.NoError(t, first.Close(), "release the lock as a finished process would")

	second, err := manifest.Load(path)
	require.NoError(t, err)

	second.StartRun()
	second.RecordDone("b", manifest.Signature{Size: 1})
	// Flush without finishing the run: b lands in the log, a stays in the snapshot.
	require.NoError(t, second.Flush())

	_, logFile := rootShardFiles(path)

	_, statErr := os.Stat(logFile)
	require.NoError(t, statErr, "the second run's delta is in the log")

	// A reload merges both sources.
	require.NoError(t, second.Close(), "release the lock as a finished process would")

	reloaded, err := manifest.Load(path)
	require.NoError(t, err)

	_, ok := reloaded.Entry("a")
	assert.True(t, ok, "the snapshot object survives")

	_, ok = reloaded.Entry("b")
	assert.True(t, ok, "the logged object survives")
	assert.Equal(t, 2, reloaded.Tally().Done)
}

func TestLoad_LegacyShardLogIsRefused(t *testing.T) {
	t.Parallel()

	// A per-shard log is a pre-release layout this ledger no longer reads.
	// Loading past it would silently drop the records it holds, so the load
	// refuses and names the file; the operator deletes it to accept a re-fetch
	// of whatever it recorded.
	root := t.TempDir()

	legacyLog := filepath.Join(root, "projects", "p", "workspaces", "w", ".ledger", "log.ndjson")
	legacyLine := `{"kind":"entry","path":"projects/p/workspaces/w/runs/r1/run.json",` +
		`"entry":{"firstSeen":"2026-07-08T12:00:00Z","status":"errored","attempts":1}}`

	require.NoError(t, os.MkdirAll(filepath.Dir(legacyLog), 0o755))
	require.NoError(t, os.WriteFile(legacyLog, []byte(legacyLine+"\n"), 0o600))

	_, err := manifest.Load(root)
	require.ErrorIs(t, err, manifest.ErrLegacyLayout)
	require.ErrorContains(t, err, legacyLog, "the refusal names the file to delete")

	// Deleting the named file is the accepted path forward, and it releases
	// the refusal (and the lock the failed load never leaked).
	require.NoError(t, os.Remove(legacyLog))

	ledger, err := manifest.Load(root)
	require.NoError(t, err)
	require.NoError(t, ledger.Close())
}

func TestLoad_TornTrailingLogLineDropped(t *testing.T) {
	t.Parallel()

	path := t.TempDir()

	// A committed record (newline-terminated) followed by a torn trailing write
	// with no commit marker: the fragment is dropped, the committed record stays.
	good := `{"kind":"entry","path":"ws/a.json","entry":{"firstSeen":"2026-07-08T12:00:00Z","status":"done","attempts":1}}`
	torn := `{"kind":"entry","path":"ws/b.json","entry":{"firstSeen"`
	_, logFile := rootShardFiles(path)
	require.NoError(t, os.MkdirAll(filepath.Dir(logFile), 0o755))
	require.NoError(t, os.WriteFile(logFile, []byte(good+"\n"+torn), 0o600))

	ledger, err := manifest.Load(path)
	require.NoError(t, err)

	_, ok := ledger.Entry("ws/a.json")
	assert.True(t, ok, "the committed record is applied")

	_, ok = ledger.Entry("ws/b.json")
	assert.False(t, ok, "the torn trailing record is dropped")
}

func TestLoad_UnparsableLogLineTruncatesTornSuffix(t *testing.T) {
	t.Parallel()

	path := t.TempDir()

	// A complete, newline-terminated line that does not parse marks where a
	// power loss stopped persisting a flush batch, so the load truncates the log
	// there instead of refusing the whole manifest: the records before it stay,
	// the ones after it (persisted out of order) are dropped and re-derived by
	// re-walking.
	good := `{"kind":"entry","path":"ws/a.json","entry":{"firstSeen":"2026-07-08T12:00:00Z","status":"done","attempts":1}}`
	garbled := string(make([]byte, 32))
	survivor := `{"kind":"entry","path":"ws/b.json","entry":{"firstSeen":"2026-07-08T12:00:00Z","status":"done","attempts":1}}`
	_, logFile := rootShardFiles(path)
	require.NoError(t, os.MkdirAll(filepath.Dir(logFile), 0o755))
	require.NoError(t, os.WriteFile(logFile,
		[]byte(good+"\n"+garbled+"\n"+survivor+"\n"), 0o600))

	ledger, err := manifest.Load(path)
	require.NoError(t, err)

	_, ok := ledger.Entry("ws/a.json")
	assert.True(t, ok, "the record before the torn suffix is applied")

	_, ok = ledger.Entry("ws/b.json")
	assert.False(t, ok, "the record past the torn suffix is dropped")

	// The truncated log keeps accepting appends at the cut: a fresh record
	// flushes, and a reload replays both it and the surviving prefix.
	ledger.RecordDone("ws/c.json", manifest.Signature{Size: 1})
	require.NoError(t, ledger.Flush())
	require.NoError(t, ledger.Close())

	reloaded, err := manifest.Load(path)
	require.NoError(t, err)

	_, ok = reloaded.Entry("ws/a.json")
	assert.True(t, ok, "the surviving prefix outlives the truncation")

	_, ok = reloaded.Entry("ws/c.json")
	assert.True(t, ok, "the post-truncation append is committed")
}

func TestLedger_ShardsPerWorkspace(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordDone("projects/p1/workspaces/ws1/runs/r1/run.json", manifest.Signature{Size: 1})
	ledger.RecordDone("projects/p1/workspaces/ws2/state-versions/s.meta.json", manifest.Signature{Size: 1})
	ledger.RecordDone("org.json", manifest.Signature{Size: 1})
	ledger.FinishRun()
	require.NoError(t, ledger.Flush())

	// Each workspace's entries compact into a shard co-located with its subtree,
	// and the org-level object into the org-root shard.
	for _, dir := range []string{
		filepath.Join(root, "projects", "p1", "workspaces", "ws1", ".ledger"),
		filepath.Join(root, "projects", "p1", "workspaces", "ws2", ".ledger"),
		filepath.Join(root, ".ledger"),
	} {
		_, statErr := os.Stat(filepath.Join(dir, "snapshot.json"))
		require.NoError(t, statErr, "a shard snapshot exists under %s", dir)
	}

	// A reload discovers every shard by globbing the known depths and rebuilds the
	// full state.
	require.NoError(t, ledger.Close(), "release the lock as a finished process would")

	reloaded, err := manifest.Load(root)
	require.NoError(t, err)

	for _, relPath := range []string{
		"projects/p1/workspaces/ws1/runs/r1/run.json",
		"projects/p1/workspaces/ws2/state-versions/s.meta.json",
		"org.json",
	} {
		_, ok := reloaded.Entry(relPath)
		assert.True(t, ok, "entry %q survives the shard round-trip", relPath)
	}

	assert.Equal(t, 3, reloaded.Tally().Done)
}

func TestLedger_FoldRewritesOnlyChangedShards(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordDone("projects/p/workspaces/ws1/runs/r/run.json", manifest.Signature{Size: 1})
	ledger.RecordDone("projects/p/workspaces/ws2/runs/r/run.json", manifest.Signature{Size: 1})
	ledger.FinishRun()
	require.NoError(t, ledger.Flush())

	// A resumed run that records only into ws1 leaves ws2's shard untouched:
	// mid-run flushes append the delta to the org log only, and the finished
	// run's fold rewrites only the snapshots the log made stale.
	require.NoError(t, ledger.Close(), "release the lock as a finished process would")

	ws1Snap := filepath.Join(root, "projects", "p", "workspaces", "ws1", ".ledger", "snapshot.json")
	ws2Snap := filepath.Join(root, "projects", "p", "workspaces", "ws2", ".ledger", "snapshot.json")
	orgLog := filepath.Join(root, ".ledger", "log.ndjson")

	ws2Before, err := os.ReadFile(ws2Snap)
	require.NoError(t, err)

	resumed, err := manifest.Load(root)
	require.NoError(t, err)

	resumed.StartRun()
	resumed.RecordErrored("projects/p/workspaces/ws1/runs/r2/run.json", errors.New("boom"), true)
	require.NoError(t, resumed.Flush())

	data, err := os.ReadFile(orgLog)
	require.NoError(t, err, "a mid-run flush appends the delta to the org log")
	assert.Contains(t, string(data), "r2/run.json")

	ws1Before, err := os.ReadFile(ws1Snap)
	require.NoError(t, err)
	assert.NotContains(t, string(ws1Before), "r2/run.json", "a mid-run flush rewrites no snapshot")

	resumed.FinishRun()
	require.NoError(t, resumed.Flush())
	require.NoError(t, resumed.Close(), "release the lock as a finished process would")

	ws1After, err := os.ReadFile(ws1Snap)
	require.NoError(t, err)
	assert.Contains(t, string(ws1After), "r2/run.json", "the fold captures the delta in the stale shard")

	ws2After, err := os.ReadFile(ws2Snap)
	require.NoError(t, err)
	assert.Equal(t, string(ws2Before), string(ws2After), "the untouched workspace's snapshot is not rewritten")

	_, statErr := os.Stat(orgLog)
	assert.True(t, os.IsNotExist(statErr), "the finished run's fold resets the org log")
}

func TestLedger_SettledSurvivesLogRoundTrip(t *testing.T) {
	t.Parallel()

	// The load-bearing guard for the non-monotonic settled record: a walSettled
	// value logged as false must replay as false, not as the record kind's
	// implicit true. Two mid-run flushes keep both records in the log (no
	// compaction), so the reload exercises the replay path.
	path := t.TempDir()

	ledger, err := manifest.Load(path)
	require.NoError(t, err)

	ledger.StartRun()
	ledger.Collection("runs").SetSettled(true)
	require.NoError(t, ledger.Flush())

	// The true value is durable on its own before the flip, so the final false is
	// a genuine true->false transition rather than a value that was never set.
	require.NoError(t, ledger.Close(), "release the lock as a finished process would")

	afterTrue, err := manifest.Load(path)
	require.NoError(t, err)
	assert.True(t, afterTrue.Collection("runs").Settled(), "settled=true persists through the log")

	ledger.Collection("runs").SetSettled(false)
	require.NoError(t, ledger.Flush())

	require.NoError(t, afterTrue.Close(), "release the lock as a finished process would")

	reloaded, err := manifest.Load(path)
	require.NoError(t, err)
	assert.False(t, reloaded.Collection("runs").Settled(),
		"a logged settled=false replays as false, re-widening the walk")
}

func TestLedger_SettledSurvivesCompaction(t *testing.T) {
	t.Parallel()

	// Finishing the run folds the log into the snapshot, so the settled flag must
	// round-trip through the compacted document too.
	path := t.TempDir()

	ledger, err := manifest.Load(path)
	require.NoError(t, err)

	ledger.StartRun()
	ledger.Collection("runs").SetSettled(true)
	ledger.FinishRun()
	require.NoError(t, ledger.Flush())

	snapshot, logFile := rootShardFiles(path)

	_, statErr := os.Stat(snapshot)
	require.NoError(t, statErr, "compaction writes the snapshot")

	_, statErr = os.Stat(logFile)
	assert.True(t, os.IsNotExist(statErr), "the log is removed after compaction")

	require.NoError(t, ledger.Close(), "release the lock as a finished process would")

	reloaded, err := manifest.Load(path)
	require.NoError(t, err)
	assert.True(t, reloaded.Collection("runs").Settled(), "settled survives compaction into the snapshot")
}

func TestLedger_HasUnsettledUnderPrefixIsolation(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.StartRun()

	// A run child left errored and a forbidden state-version blob sit in the same
	// workspace shard as a fully archived state version. HasUnsettledUnder must
	// isolate on the prefix so the runs walk sees its errored child while the
	// state-versions walk sees only settled work.
	const (
		runsKey  = "projects/p/workspaces/w/runs"
		svKey    = "projects/p/workspaces/w/state-versions"
		otherKey = "projects/p/workspaces/w/configuration-versions"
	)

	ledger.RecordErrored(runsKey+"/r1/plan.log", errors.New("boom"), true)
	ledger.RecordDone(runsKey+"/r1/run.json", manifest.Signature{Size: 1})
	ledger.RecordDone(svKey+"/sv.meta.json", manifest.Signature{Size: 1})

	assert.True(t, ledger.Collection(runsKey).HasUnsettled(), "the errored run child is unsettled under runs")
	assert.False(t, ledger.Collection(svKey).HasUnsettled(), "the state-versions collection has only settled work")
	assert.False(t, ledger.Collection(otherKey).HasUnsettled(), "a prefix with no entries is settled")

	// A forbidden state-version child flips its collection to unsettled without
	// touching the runs prefix.
	ledger.RecordForbidden(svKey+"/sv2.tfstate.json", errors.New("forbidden"))
	assert.True(t, ledger.Collection(svKey).HasUnsettled(), "the forbidden state-version child is unsettled")
}

func TestLedger_HasUnsettledUnderRetryAbsent(t *testing.T) {
	t.Parallel()

	const runsKey = "projects/p/workspaces/w/runs"

	root := t.TempDir()

	// The workspace subtree exists before records reference it, as in a real
	// run where the object files land first.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects", "p", "workspaces", "w"), 0o755))

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	ledger.RecordDone(runsKey+"/r1/run.json", manifest.Signature{Size: 1})
	ledger.RecordAbsent(runsKey+"/r1/plan.json", errors.New("404"))
	require.NoError(t, ledger.Flush())
	require.NoError(t, ledger.Close())

	normal, err := manifest.Load(root)
	require.NoError(t, err)
	assert.False(t, normal.Collection(runsKey).HasUnsettled(),
		"a recorded absence is settled for a normal run")
	require.NoError(t, normal.Close())

	// Under retry-absent the absence counts as unsettled, so a settled
	// collection's early stop re-opens and the re-walk actually reaches the
	// absent children the flag exists to re-probe.
	retry, err := manifest.Load(root, manifest.WithRetryAbsent(true))
	require.NoError(t, err)
	assert.True(t, retry.Collection(runsKey).HasUnsettled(),
		"a recorded absence is unsettled under retry-absent")
	require.NoError(t, retry.Close())
}

func TestLedger_HasUnsettledUnderMatchesDeepDescendant(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.StartRun()

	// A stack configuration walk gates on the configurations prefix, and its
	// deployment-group run steps nest many levels beneath it. An errored step
	// artifact deep under the prefix must still be caught, so the configuration
	// walk re-pages and re-reaches it through the composed sub-walks.
	const (
		configPrefix = "projects/p/stacks/s/configurations"
		deepChild    = configPrefix + "/cfg1/deployment-groups/g1/runs/r1/steps/st1/plan-description.json"
	)

	ledger.RecordDone(configPrefix+"/cfg1/configuration.json", manifest.Signature{Size: 1})
	ledger.RecordErrored(deepChild, errors.New("boom"), true)

	assert.True(t, ledger.Collection(configPrefix).HasUnsettled(),
		"a deeply nested errored child is caught by the shallow configurations prefix")
}

func TestLedger_HasUnsettledScopedToItsPrefix(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.StartRun()

	// The gate scans only entries beneath the handle's own prefix, so an
	// unsettled child in one collection never holds a sibling collection open.
	ledger.RecordErrored("projects/p/stacks/s/configurations/c/config.json", errors.New("boom"), true)

	assert.True(t, ledger.Collection("projects/p/stacks/s/configurations").HasUnsettled(),
		"the owning collection sees its errored child")
	assert.False(t, ledger.Collection("projects/p/stacks/other/configurations").HasUnsettled(),
		"a sibling collection does not")
}

func TestLedger_MirrorReferenceGate(t *testing.T) {
	t.Parallel()

	const (
		runsPrefix = "projects/p/workspaces/w/runs"
		gate       = runsPrefix + "/r1/created-by.ref"
	)

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.StartRun()

	// A reference that never failed creates no entry: mirroring a settled write
	// against an absent gate is a no-op, so the happy path leaves no *.ref bloat.
	ledger.MirrorReference(gate, true)

	_, ok := ledger.Entry(gate)
	assert.False(t, ok, "a settled reference against an absent gate creates no entry")
	assert.False(t, ledger.ReferencePending(gate))
	assert.False(t, ledger.Collection(runsPrefix).HasUnsettled())

	// An unsettled foreign write opens the gate as pending: unsettled, but not a
	// failure. HasUnsettledUnder now sees outstanding work under the runs prefix
	// even though the write itself lives in a sibling shard.
	ledger.MirrorReference(gate, false)

	entry, ok := ledger.Entry(gate)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusPending, entry.Status)
	assert.True(t, ledger.ReferencePending(gate))
	assert.True(t, ledger.Collection(runsPrefix).HasUnsettled(),
		"a pending gate makes its run unsettled under the runs prefix")

	// A pending gate never inflates the error tally, the total, or the failure
	// list: it is absent from Tally's six counters and from Failures.
	tally := ledger.Tally()
	assert.Equal(t, 0, tally.Errored, "pending is not an error")
	assert.Equal(t, 0, tally.Total(), "pending is not one of the counted statuses")
	assert.Empty(t, ledger.Failures(), "a pending gate is not a failure")

	// Re-mirroring the same unsettled write is idempotent: an already-pending gate
	// is not re-dirtied, so a re-walk does not climb its attempt count.
	ledger.MirrorReference(gate, false)

	entry, ok = ledger.Entry(gate)
	require.True(t, ok)
	assert.Equal(t, 1, entry.Attempts, "an already-pending gate is not re-recorded")

	// The foreign write settling clears the gate to not-applicable, a settled state
	// that stops the retry.
	ledger.MirrorReference(gate, true)

	entry, ok = ledger.Entry(gate)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusReferenceCleared, entry.Status)
	assert.False(t, ledger.ReferencePending(gate))
	assert.False(t, ledger.Collection(runsPrefix).HasUnsettled(), "a cleared gate settles the runs prefix")

	// A cleared gate is a ledger-only proxy, not a real object, so it stays out
	// of the tally's object counters just as the pending gate did: a recovered
	// reference must not inflate the not-applicable or total counts.
	clearedTally := ledger.Tally()
	assert.Equal(t, 0, clearedTally.NotApplicable, "a cleared gate is not a not-applicable object")
	assert.Equal(t, 0, clearedTally.Total(), "a cleared gate is not one of the counted statuses")

	// A cleared gate is not re-dirtied by a later settled re-mirror.
	cleared := entry.Attempts

	ledger.MirrorReference(gate, true)

	entry, _ = ledger.Entry(gate)
	assert.Equal(t, cleared, entry.Attempts, "a cleared gate is not re-dirtied on later re-walks")
}

func TestLedger_ReferencePendingAbsentIsFalse(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	// Unlike ShouldFetch, an absent gate is not pending: its absence means no
	// outstanding work, so the actor re-read is not forced when no gate was written.
	assert.False(t, ledger.ReferencePending("projects/p/workspaces/w/runs/r1/x.ref"))
	assert.True(t, ledger.ShouldFetch("projects/p/workspaces/w/runs/r1/x.ref"),
		"the two predicates differ on an absent path")
}

func TestLedger_PendingGateSurvivesReload(t *testing.T) {
	t.Parallel()

	path := t.TempDir()

	const (
		runsPrefix = "projects/p/workspaces/w/runs"
		gate       = runsPrefix + "/r1/run-events-actors.ref"
	)

	// The workspace subtree exists before records reference it, as in a real
	// run where the object files land first.
	require.NoError(t, os.MkdirAll(filepath.Join(path, "projects", "p", "workspaces", "w"), 0o755))

	ledger, err := manifest.Load(path)
	require.NoError(t, err)

	ledger.StartRun()
	ledger.MirrorReference(gate, false)
	require.NoError(t, ledger.Flush())

	// A snapshot carrying a pending gate loads cleanly (Valid accepts it) and the
	// gate still forces its run unsettled on resume.
	require.NoError(t, ledger.Close(), "release the lock as a finished process would")

	reloaded, err := manifest.Load(path)
	require.NoError(t, err)

	entry, ok := reloaded.Entry(gate)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusPending, entry.Status)
	assert.True(t, reloaded.Collection(runsPrefix).HasUnsettled(), "a pending gate re-widens the walk on resume")
}

func TestLedger_DroppedSurfaces(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.StartRun()

	assert.Zero(t, ledger.Tally().SurfacesDropped, "a fresh run starts with no dropped surfaces")

	ledger.MarkSurfaceDropped("registry/modules", errors.New("list modules: boom"))
	ledger.MarkSurfaceDropped("workspaces", errors.New("list workspaces: boom"))
	ledger.MarkSurfaceDropped("registry/modules", errors.New("list modules: boom again"))

	tally := ledger.Tally()
	assert.Equal(t, 2, tally.SurfacesDropped, "re-dropping the same surface counts once")

	dropped := ledger.DroppedSurfaces()
	require.Len(t, dropped, 2)
	assert.Equal(t, "registry/modules", dropped[0].Surface, "sorted by surface")
	assert.Equal(t, "list modules: boom again", dropped[0].Error, "the latest cause wins")
	assert.Equal(t, "workspaces", dropped[1].Surface)

	// The record is run-scoped: a new run retries every enumeration from
	// scratch, so the next run starts clean rather than inheriting a drop it
	// might not repeat.
	ledger.StartRun()
	assert.Zero(t, ledger.Tally().SurfacesDropped)
	assert.Empty(t, ledger.DroppedSurfaces())
}

func TestLedger_RecordAbsentSettlesWithCause(t *testing.T) {
	t.Parallel()

	const relPath = "projects/p/workspaces/w/runs/r1/run.json"

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordAbsent(relPath, errors.New("resource not found"))

	// The absence settles immediately and keeps its cause, so the manifest
	// documents why the gap exists.
	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusAbsent, entry.Status)
	assert.Equal(t, "resource not found", entry.LastError)
	assert.False(t, ledger.ShouldFetch(relPath), "a recorded absence is settled")

	// A recorded absence never surfaces as a failure: it is a settled gap, not
	// an error awaiting a retry.
	assert.Empty(t, ledger.Failures())
}

func TestLedger_LockExcludesConcurrentOpen(t *testing.T) {
	t.Parallel()

	path := t.TempDir()

	first, err := manifest.Load(path)
	require.NoError(t, err)

	// A second open while the first is live must fail fast: the WAL, compaction,
	// and tallies all assume one writer, so a scheduled run starting while the
	// previous still runs cannot be allowed to interleave.
	_, err = manifest.Load(path)
	require.ErrorIs(t, err, manifest.ErrLedgerLocked)

	// Releasing the lock is what a process death does implicitly (the kernel
	// drops an flock when its descriptors close), so a crashed holder never
	// blocks a later run and no stale-lock recovery exists.
	require.NoError(t, first.Close())

	second, err := manifest.Load(path)
	require.NoError(t, err)
	require.NoError(t, second.Close())

	// Close is idempotent, so a belt-and-braces double close is harmless.
	require.NoError(t, second.Close())
}

func TestLoad_SymlinkAliasSharesOneShardAcrossRuns(t *testing.T) {
	t.Parallel()

	// A workspace renamed in the API (alpha -> zeta) whose operator left a
	// rename symlink beside the old directory: the first run archived under
	// alpha, later runs record under zeta, and both keys reach one physical
	// .ledger directory. The alias must join alpha's shard rather than open a
	// second one over the same snapshot file, where whichever alias folded
	// second would overwrite the other's history after the org log carrying it
	// was already retired.
	root := t.TempDir()

	const (
		alphaEntry = "projects/p/workspaces/alpha/runs/r1/run.json"
		zetaEntry  = "projects/p/workspaces/zeta/runs/r2/run.json"
	)

	first, err := manifest.Load(root)
	require.NoError(t, err)

	first.StartRun()
	first.RecordDone(alphaEntry, manifest.Signature{Size: 1})
	first.FinishRun()
	require.NoError(t, first.Flush())
	require.NoError(t, first.Close())

	wsDir := filepath.Join(root, "projects", "p", "workspaces")
	require.NoError(t, os.Symlink(
		filepath.Join(wsDir, "alpha"),
		filepath.Join(wsDir, "zeta"),
	))

	second, err := manifest.Load(root)
	require.NoError(t, err)

	second.StartRun()
	second.RecordDone(zetaEntry, manifest.Signature{Size: 1})
	second.FinishRun()
	require.NoError(t, second.Flush())
	require.NoError(t, second.Close())

	third, err := manifest.Load(root)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, third.Close()) })

	assert.False(t, third.ShouldFetch(alphaEntry),
		"the first run's record survives the aliased fold")
	assert.False(t, third.ShouldFetch(zetaEntry),
		"the record under the alias key resolves on reload")
	assert.Equal(t, 2, third.Tally().Done,
		"the shared shard's entries seed the tally once")
}

func TestLedger_RecordTimeAliasJoinsTheExistingShard(t *testing.T) {
	t.Parallel()

	// The aliased directory can also appear before any shard exists beneath
	// it: the object files land first, so the workspace directory and its
	// rename symlink exist while the .ledger they imply does not. A record
	// under each key inside one run must still share a shard, resolved through
	// the deepest existing ancestor.
	root := t.TempDir()

	wsDir := filepath.Join(root, "projects", "p", "workspaces")
	require.NoError(t, os.MkdirAll(filepath.Join(wsDir, "alpha"), 0o755))
	require.NoError(t, os.Symlink(
		filepath.Join(wsDir, "alpha"),
		filepath.Join(wsDir, "zeta"),
	))

	const (
		alphaEntry = "projects/p/workspaces/alpha/runs/r1/run.json"
		zetaEntry  = "projects/p/workspaces/zeta/runs/r2/run.json"
	)

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordDone(alphaEntry, manifest.Signature{Size: 1})
	ledger.RecordDone(zetaEntry, manifest.Signature{Size: 1})
	ledger.FinishRun()
	require.NoError(t, ledger.Flush())
	require.NoError(t, ledger.Close())

	reloaded, err := manifest.Load(root)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, reloaded.Close()) })

	assert.False(t, reloaded.ShouldFetch(alphaEntry))
	assert.False(t, reloaded.ShouldFetch(zetaEntry))
	assert.Equal(t, 2, reloaded.Tally().Done)
}

func TestLoad_DeletedSubtreeDiscardsItsLogRecords(t *testing.T) {
	t.Parallel()

	// Deleting an archived subtree demands a re-archive. The subtree's shard
	// snapshot dies with its co-located .ledger directory, but a crashed run's
	// org-level log still names the entries, and replaying them would freeze
	// the deletion by answering every fetch decision with a skip. The replay
	// discards them instead, so the subtree re-fetches, while records for
	// surviving shards replay untouched.
	root := t.TempDir()

	const (
		wsEntry  = "projects/p/workspaces/w/runs/r1/run.json"
		orgEntry = "org.json"
	)

	wsDir := filepath.Join(root, "projects", "p", "workspaces", "w")
	require.NoError(t, os.MkdirAll(wsDir, 0o755))

	first, err := manifest.Load(root)
	require.NoError(t, err)

	first.StartRun()
	first.RecordDone(wsEntry, manifest.Signature{Size: 1})
	first.RecordDone(orgEntry, manifest.Signature{Size: 1})

	// Flush without finishing the run, as an interrupted process would have:
	// the records stay in the org-level log rather than folding into
	// snapshots.
	require.NoError(t, first.Flush())
	require.NoError(t, first.Close())

	require.NoError(t, os.RemoveAll(wsDir))

	reloaded, err := manifest.Load(root)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, reloaded.Close()) })

	assert.True(t, reloaded.ShouldFetch(wsEntry),
		"the deleted subtree's records discard, so it re-archives")
	assert.False(t, reloaded.ShouldFetch(orgEntry),
		"records for surviving shards replay")
	assert.Equal(t, 1, reloaded.Tally().Done,
		"the discarded records do not seed the tally")
	assert.Equal(t, 1, reloaded.Tally().RecordsDiscarded,
		"the discard surfaces in the summary tally, not only the log")
}

func TestLoad_RecordOnlyPrefixKeepsRecordsWithoutFiles(t *testing.T) {
	t.Parallel()

	// An evicted surface's ledger entries are the only local proof its remote
	// copies exist: the subtree can be sparse or absent without that absence
	// meaning anything. A declared record-only prefix therefore replays even
	// with no directory on disk, while the same records discard when the
	// prefix is not declared.
	root := t.TempDir()

	const tarball = "config-versions/cv-1.tar.gz"

	first, err := manifest.Load(root)
	require.NoError(t, err)

	first.StartRun()
	first.RecordDone(tarball, manifest.Signature{Size: 1})

	// Flush without finishing the run, as an interrupted process would have,
	// so the record stays in the org-level log; the config-versions directory
	// itself is never created, as full eviction would leave it.
	require.NoError(t, first.Flush())
	require.NoError(t, first.Close())

	plain, err := manifest.Load(root)
	require.NoError(t, err)
	assert.True(t, plain.ShouldFetch(tarball),
		"an undeclared prefix's records discard with the missing subtree")
	require.NoError(t, plain.Close())

	declared, err := manifest.Load(root,
		manifest.WithRecordOnlyPrefixes("config-versions"))
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, declared.Close()) })

	assert.False(t, declared.ShouldFetch(tarball),
		"a record-only prefix's records replay without any local files")
}

func TestLedger_EntryDurableTracksTheFlushLifecycle(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root defeats the read-only directory that forces the append failure")
	}

	// Custody decisions gate on durability: an eviction may destroy a local
	// only-copy only once the done record proving the remote copy would
	// survive a crash. The signal must follow the flush lifecycle -- false
	// while the record lives only in memory, true once the fsynced log holds
	// it, and false again when a failed append hands the record back.
	root := t.TempDir()

	const tarball = "config-versions/cv-1.tar.gz"

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, ledger.Close()) })

	assert.False(t, ledger.EntryDurable(tarball), "no entry at all")

	ledger.RecordDone(tarball, manifest.Signature{Size: 1})
	assert.False(t, ledger.EntryDurable(tarball), "recorded but not yet flushed")

	require.NoError(t, ledger.Flush())
	assert.True(t, ledger.EntryDurable(tarball), "the fsynced log holds the record")

	ledger.RecordDone(tarball, manifest.Signature{Size: 2})
	assert.False(t, ledger.EntryDurable(tarball), "a re-record dirties the entry again")

	// A failed append restores the drained delta, so the entry must read
	// non-durable rather than silently durable-without-a-log. A read-only
	// log file makes the append's open fail while leaving everything else
	// intact.
	_, logFile := rootShardFiles(root)
	require.NoError(t, os.Chmod(logFile, 0o400))
	require.Error(t, ledger.Flush())
	require.NoError(t, os.Chmod(logFile, 0o600))

	assert.False(t, ledger.EntryDurable(tarball), "the failed append handed the record back")

	require.NoError(t, ledger.Flush())
	assert.True(t, ledger.EntryDurable(tarball), "the retried flush lands it")
}
