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

	path := filepath.Join(t.TempDir(), "manifest.json")

	ledger, err := manifest.Load(path)
	require.NoError(t, err)

	_, ok := ledger.Entry("anything")
	assert.False(t, ok)
	assert.Equal(t, 0, ledger.RunCount())
	assert.True(t, ledger.ShouldFetch("anything"))
}

func TestLoad_Corrupt(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "manifest.json")
	require.NoError(t, os.WriteFile(path, []byte("{ not json"), 0o600))

	_, err := manifest.Load(path)
	require.ErrorIs(t, err, manifest.ErrCorruptManifest)
}

func TestLoad_UnknownStatus(t *testing.T) {
	t.Parallel()

	// A status outside the recognized set would seed the cumulative tally under a
	// key the tally never reads, so a resumed run would report a zero total; the
	// load rejects it instead.
	path := filepath.Join(t.TempDir(), "manifest.json")
	doc := `{"version":1,"entries":{"a":{"status":"pending","attempts":1}}}`
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	_, err := manifest.Load(path)
	require.ErrorIs(t, err, manifest.ErrCorruptManifest)
}

func TestLedger_RoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 8, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "manifest.json")

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

			plain, err := manifest.Load(filepath.Join(t.TempDir(), "m.json"))
			require.NoError(t, err)
			tc.record(plain, "obj")
			assert.Equal(t, tc.wantNoRecheck, plain.ShouldFetch("obj"))

			recheck, err := manifest.Load(
				filepath.Join(t.TempDir(), "m.json"),
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

	ledger, err := manifest.Load(filepath.Join(t.TempDir(), "m.json"))
	require.NoError(t, err)

	assert.True(t, ledger.ShouldFetch("never-seen"))
}

func TestLedger_HighWaterMarkMonotonic(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(filepath.Join(t.TempDir(), "m.json"))
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

	ledger, err := manifest.Load(filepath.Join(t.TempDir(), "m.json"))
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

	tally := ledger.Tally()
	assert.Equal(t, 2, tally.Done)
	assert.Equal(t, 1, tally.Errored)
	assert.Equal(t, 1, tally.Forbidden)
	assert.Equal(t, 1, tally.Skipped)
	assert.Equal(t, 1, tally.AbsentPermanently)
	assert.Equal(t, 1, tally.NotApplicable)
	assert.Equal(t, int64(42), tally.BytesDownloaded)
	assert.Equal(t, 7, tally.Total())
}

func TestLedger_CumulativeAcrossRuns(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "m.json")

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

	path := filepath.Join(t.TempDir(), "m.json")

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
	second, err := manifest.Load(path)
	require.NoError(t, err)

	second.StartRun()
	assert.Equal(t, 1, second.Tally().Done, "carried over once")

	second.RecordDone("m", manifest.Signature{Size: 1})
	assert.Equal(t, 1, second.Tally().Done, "re-read on resume does not double-count")
}

func TestLedger_ReRecordSwapsTally(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(filepath.Join(t.TempDir(), "m.json"))
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

	ledger, err := manifest.Load(filepath.Join(t.TempDir(), "m.json"))
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

	ledger, err := manifest.Load(filepath.Join(t.TempDir(), "m.json"))
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

	ledger, err := manifest.Load(filepath.Join(t.TempDir(), "m.json"))
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

	ledger, err := manifest.Load(filepath.Join(t.TempDir(), "m.json"))
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
