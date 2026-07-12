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
	assert.Equal(t, "absent-permanently", manifest.StatusAbsentPermanently.String())
	assert.Equal(t, "forbidden", manifest.StatusForbidden.String())
	assert.Equal(t, "not-applicable", manifest.StatusNotApplicable.String())
}

func TestStatus_ValidAndSettled(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status      manifest.Status
		wantValid   bool
		wantSettled bool
	}{
		"done":      {status: manifest.StatusDone, wantValid: true, wantSettled: true},
		"absent":    {status: manifest.StatusAbsentPermanently, wantValid: true, wantSettled: true},
		"skipped":   {status: manifest.StatusSkipped, wantValid: true, wantSettled: true},
		"na":        {status: manifest.StatusNotApplicable, wantValid: true, wantSettled: true},
		"errored":   {status: manifest.StatusErrored, wantValid: true, wantSettled: false},
		"forbidden": {status: manifest.StatusForbidden, wantValid: true, wantSettled: false},
		"pending":   {status: manifest.StatusPending, wantValid: true, wantSettled: false},
		"cleared":   {status: manifest.StatusReferenceCleared, wantValid: true, wantSettled: true},
		"unknown":   {status: manifest.Status("nonsense"), wantValid: false, wantSettled: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.wantValid, tc.status.Valid())
			assert.Equal(t, tc.wantSettled, tc.status.Settled())
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
	ledger.RecordAbsent("ws/expired.tar.gz")
	ledger.RecordErrored("ws/run.log", errors.New("boom"), true)
	ledger.RecordForbidden("github-app-installations.json", errors.New("forbidden: team tokens not supported"))
	ledger.RecordSkipped("ws/deferred.json")
	ledger.RecordNotApplicable("ws/ssh-keys.json")
	ledger.AdvanceHighWaterMark("state-versions", now)
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
	assert.Equal(t, now, reloaded.HighWaterMark("state-versions"))

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
		record        func(l *manifest.Ledger, path string)
		wantNoRecheck bool
		wantRecheck   bool
	}{
		"done is settled": {
			record:        func(l *manifest.Ledger, p string) { l.RecordDone(p, manifest.Signature{Size: 1}) },
			wantNoRecheck: false,
			wantRecheck:   false,
		},
		"skipped is settled": {
			record:        func(l *manifest.Ledger, p string) { l.RecordSkipped(p) },
			wantNoRecheck: false,
			wantRecheck:   false,
		},
		"not-applicable is settled": {
			record:        func(l *manifest.Ledger, p string) { l.RecordNotApplicable(p) },
			wantNoRecheck: false,
			wantRecheck:   false,
		},
		"errored is retried": {
			record:        func(l *manifest.Ledger, p string) { l.RecordErrored(p, errors.New("x"), false) },
			wantNoRecheck: true,
			wantRecheck:   true,
		},
		"forbidden is retried": {
			record:        func(l *manifest.Ledger, p string) { l.RecordForbidden(p, errors.New("forbidden")) },
			wantNoRecheck: true,
			wantRecheck:   true,
		},
		"absent is sticky but recheckable": {
			record:        func(l *manifest.Ledger, p string) { l.RecordAbsent(p) },
			wantNoRecheck: false,
			wantRecheck:   true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			plain, err := manifest.Load(t.TempDir())
			require.NoError(t, err)
			tc.record(plain, "obj")
			assert.Equal(t, tc.wantNoRecheck, plain.ShouldFetch("obj"))

			recheck, err := manifest.Load(
				t.TempDir(),
				manifest.WithRecheckAbsent(true),
			)
			require.NoError(t, err)
			tc.record(recheck, "obj")
			assert.Equal(t, tc.wantRecheck, recheck.ShouldFetch("obj"))
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

	ledger.AdvanceHighWaterMark("runs", late)
	assert.Equal(t, late, ledger.HighWaterMark("runs"))

	// An earlier value never moves the mark backward.
	ledger.AdvanceHighWaterMark("runs", early)
	assert.Equal(t, late, ledger.HighWaterMark("runs"))

	assert.True(t, ledger.HighWaterMark("unset").IsZero())
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
	ledger.RecordAbsent("e")
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
	assert.Equal(t, 1, tally.AbsentPermanently)
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
				ledger.AdvanceHighWaterMark("runs", time.Unix(int64(i), 0))

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

	ledger, err := manifest.Load(path)
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordDone("projects/p/workspaces/w/runs/r/run.json", manifest.Signature{Size: 1})
	ledger.RecordDone("org.json", manifest.Signature{Size: 2})

	// A read-only root makes every shard's log append fail; the delta a flush
	// drained up front must be restored rather than dropped.
	require.NoError(t, os.Chmod(path, 0o500))
	require.Error(t, ledger.Flush(), "an append into a read-only root must fail")

	// A retry after the write barrier lifts must re-append the same delta, so
	// the records the failed flush drained are still there to persist.
	require.NoError(t, os.Chmod(path, 0o755))
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

func TestLoad_CorruptLogLine(t *testing.T) {
	t.Parallel()

	path := t.TempDir()

	// A complete, newline-terminated line that does not parse is genuine
	// corruption, not a torn tail.
	_, logFile := rootShardFiles(path)
	require.NoError(t, os.MkdirAll(filepath.Dir(logFile), 0o755))
	require.NoError(t, os.WriteFile(logFile, []byte("{ not json\n"), 0o600))

	_, err := manifest.Load(path)
	require.ErrorIs(t, err, manifest.ErrCorruptManifest)
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

func TestLedger_FlushTouchesOnlyChangedShards(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordDone("projects/p/workspaces/ws1/runs/r/run.json", manifest.Signature{Size: 1})
	ledger.RecordDone("projects/p/workspaces/ws2/runs/r/run.json", manifest.Signature{Size: 1})
	ledger.FinishRun()
	require.NoError(t, ledger.Flush())

	// A resumed run that records only into ws1 leaves ws2's shard untouched: only
	// the shard that changed grows a log.
	require.NoError(t, ledger.Close(), "release the lock as a finished process would")

	resumed, err := manifest.Load(root)
	require.NoError(t, err)

	resumed.StartRun()
	resumed.RecordErrored("projects/p/workspaces/ws1/runs/r2/run.json", errors.New("boom"), true)
	require.NoError(t, resumed.Flush())

	ws1Log := filepath.Join(root, "projects", "p", "workspaces", "ws1", ".ledger", "log.ndjson")
	ws2Log := filepath.Join(root, "projects", "p", "workspaces", "ws2", ".ledger", "log.ndjson")

	_, statErr := os.Stat(ws1Log)
	require.NoError(t, statErr, "the touched workspace's shard grows a log")

	_, statErr = os.Stat(ws2Log)
	assert.True(t, os.IsNotExist(statErr), "the untouched workspace's shard is not rewritten")
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
	ledger.SetCollectionSettled("runs", true)
	require.NoError(t, ledger.Flush())

	// The true value is durable on its own before the flip, so the final false is
	// a genuine true->false transition rather than a value that was never set.
	require.NoError(t, ledger.Close(), "release the lock as a finished process would")

	afterTrue, err := manifest.Load(path)
	require.NoError(t, err)
	assert.True(t, afterTrue.IsCollectionSettled("runs"), "settled=true persists through the log")

	ledger.SetCollectionSettled("runs", false)
	require.NoError(t, ledger.Flush())

	require.NoError(t, afterTrue.Close(), "release the lock as a finished process would")

	reloaded, err := manifest.Load(path)
	require.NoError(t, err)
	assert.False(t, reloaded.IsCollectionSettled("runs"),
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
	ledger.SetCollectionSettled("runs", true)
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
	assert.True(t, reloaded.IsCollectionSettled("runs"), "settled survives compaction into the snapshot")
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

	assert.True(t, ledger.HasUnsettledUnder(runsKey), "the errored run child is unsettled under runs")
	assert.False(t, ledger.HasUnsettledUnder(svKey), "the state-versions collection has only settled work")
	assert.False(t, ledger.HasUnsettledUnder(otherKey), "a prefix with no entries is settled")

	// A forbidden state-version child flips its collection to unsettled without
	// touching the runs prefix.
	ledger.RecordForbidden(svKey+"/sv2.tfstate.json", errors.New("forbidden"))
	assert.True(t, ledger.HasUnsettledUnder(svKey), "the forbidden state-version child is unsettled")
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

	assert.True(t, ledger.HasUnsettledUnder(configPrefix),
		"a deeply nested errored child is caught by the shallow configurations prefix")
}

func TestLedger_HasUnsettledUnderIgnoresSyntheticCursor(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.StartRun()

	// A stack walk's cursor key is an id, not a path prefix of the entries it
	// routes past, so it matches nothing and reports settled — the documented
	// scoping that keeps stacks at today's behavior.
	ledger.RecordErrored("projects/p/stacks/s/configurations/c/config.json", errors.New("boom"), true)

	assert.False(t, ledger.HasUnsettledUnder("stacks/stk-123/configurations"),
		"a synthetic id cursor is not a prefix of any entry")
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
	assert.False(t, ledger.HasUnsettledUnder(runsPrefix))

	// An unsettled foreign write opens the gate as pending: unsettled, but not a
	// failure. HasUnsettledUnder now sees outstanding work under the runs prefix
	// even though the write itself lives in a sibling shard.
	ledger.MirrorReference(gate, false)

	entry, ok := ledger.Entry(gate)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusPending, entry.Status)
	assert.True(t, ledger.ReferencePending(gate))
	assert.True(t, ledger.HasUnsettledUnder(runsPrefix),
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
	assert.False(t, ledger.HasUnsettledUnder(runsPrefix), "a cleared gate settles the runs prefix")

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
	assert.True(t, reloaded.HasUnsettledUnder(runsPrefix), "a pending gate re-widens the walk on resume")
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

func TestLedger_AbsentObservationSecondStrike(t *testing.T) {
	t.Parallel()

	const relPath = "projects/p/workspaces/w/runs/r1/run.json"

	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	ledger, err := manifest.Load(t.TempDir(), manifest.WithClock(func() time.Time { return now }))
	require.NoError(t, err)

	ledger.StartRun()

	// The first observation is not believed: it records errored (retryable) so
	// the next run re-probes, rather than converting one possibly
	// eventually-consistent 404 into a sticky permanent absence.
	ledger.RecordAbsentObservation(relPath, errors.New("resource not found"))

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusErrored, entry.Status)
	assert.True(t, ledger.ShouldFetch(relPath), "a first observation stays retryable")

	// A repeat within the same run can sit in the same consistency window, so it
	// re-stamps without escalating.
	now = now.Add(time.Minute)

	ledger.RecordAbsentObservation(relPath, errors.New("resource not found"))

	entry, ok = ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusErrored, entry.Status)

	// A later run observing the absence again settles it.
	now = now.Add(time.Hour)

	ledger.StartRun()
	ledger.RecordAbsentObservation(relPath, errors.New("resource not found"))

	entry, ok = ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusAbsentPermanently, entry.Status)
	assert.False(t, ledger.ShouldFetch(relPath), "the confirmed absence is settled")
}

func TestLedger_AbsentObservationResetByOtherOutcomes(t *testing.T) {
	t.Parallel()

	const relPath = "projects/p/workspaces/w/runs/r1/run.json"

	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	ledger, err := manifest.Load(t.TempDir(), manifest.WithClock(func() time.Time { return now }))
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordAbsentObservation(relPath, errors.New("resource not found"))

	// A successful fetch clears the observation: the object exists, so an old
	// stamp must not pair with a much later 404.
	ledger.RecordDone(relPath, manifest.Signature{Size: 1})

	now = now.Add(time.Hour)

	ledger.StartRun()
	ledger.RecordAbsentObservation(relPath, errors.New("resource not found"))

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusErrored, entry.Status,
		"the count restarted, so this is a first observation again")

	// An intervening non-absence failure also restarts the count, so only
	// consecutive absence observations settle.
	ledger.RecordErrored(relPath, errors.New("500"), true)

	now = now.Add(time.Hour)

	ledger.StartRun()
	ledger.RecordAbsentObservation(relPath, errors.New("resource not found"))

	entry, ok = ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusErrored, entry.Status)
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
