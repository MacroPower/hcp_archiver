package manifest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dirtyEverything marks one item dirty in every category a shard tracks, so a
// drain/restore test exercises each of them without a second list of kinds to
// keep in step with the shard's fields.
func dirtyEverything(t *testing.T, sh *shard) {
	t.Helper()

	sh.entries["e1"] = &Entry{Status: StatusDone}
	sh.dirtyEntries["e1"] = struct{}{}
	sh.watermarks["w1"] = time.Unix(1, 0)
	sh.dirtyWatermarks["w1"] = struct{}{}
	sh.completed["c1"] = true
	sh.dirtyCompleted["c1"] = struct{}{}
	sh.settled["s1"] = true
	sh.dirtySettled["s1"] = struct{}{}
	sh.settled["u1"] = false
	sh.dirtySettled["u1"] = struct{}{}
	sh.dirtyUnsettled["u1"] = struct{}{}
	sh.runDirty = true
	sh.runCount = 3
}

func TestShardDrainRestoreRoundTrip(t *testing.T) {
	t.Parallel()

	sh := newShard(t.TempDir())
	dirtyEverything(t, sh)

	d := sh.drainDirty()

	assert.False(t, sh.hasDirty(), "a drain leaves the shard clean")
	require.Len(t, d.recs, 6, "one record per dirty item, plus the run record")

	// A worker re-dirties one key while the (conceptually failed) append is in
	// flight; the restore must union, not overwrite, so both the re-dirtied key
	// and the drained keys survive to the retry.
	sh.entries["e2"] = &Entry{Status: StatusErrored}
	sh.dirtyEntries["e2"] = struct{}{}

	sh.restoreDirty(d)

	assert.True(t, sh.hasDirty())
	assert.Equal(t, map[string]struct{}{"e1": {}, "e2": {}}, sh.dirtyEntries)
	assert.Equal(t, map[string]struct{}{"w1": {}}, sh.dirtyWatermarks)
	assert.Equal(t, map[string]struct{}{"c1": {}}, sh.dirtyCompleted)
	assert.Equal(t, map[string]struct{}{"s1": {}, "u1": {}}, sh.dirtySettled)
	assert.Equal(t, map[string]struct{}{"u1": {}}, sh.dirtyUnsettled)
	assert.True(t, sh.runDirty)

	// A second drain after the restore re-emits the full delta, so a retried
	// flush appends everything the failed one attempted.
	d2 := sh.drainDirty()
	assert.Len(t, d2.recs, 7, "the restored delta plus the re-dirtied entry")
	assert.False(t, sh.hasDirty())
}

func TestShardDrainLeadsUnsettleRecords(t *testing.T) {
	t.Parallel()

	// A walk unsettles a collection before recording its new entries; the
	// drain must emit that false record ahead of every entry, because the log
	// replays as a prefix after a crash and entries durable without the
	// unsettlement would leave a stale settled flag guarding a gap.
	sh := newShard(t.TempDir())

	sh.entries["runs/r4/run.json"] = &Entry{Status: StatusDone}
	sh.dirtyEntries["runs/r4/run.json"] = struct{}{}
	sh.settled["runs"] = false
	sh.dirtySettled["runs"] = struct{}{}
	sh.dirtyUnsettled["runs"] = struct{}{}

	d := sh.drainDirty()

	require.NotEmpty(t, d.recs)
	assert.Equal(t, walSettled, d.recs[0].Kind, "the unsettle record leads the batch")
	assert.Equal(t, "runs", d.recs[0].Key)
	assert.False(t, d.recs[0].Settled)

	// The still-false key already led the batch; a trailing duplicate would
	// only repeat the line.
	settledRecs := 0

	for _, rec := range d.recs {
		if rec.Kind == walSettled {
			settledRecs++
		}
	}

	assert.Equal(t, 1, settledRecs, "a still-false key emits exactly one record")
}

func TestShardDrainUnsettleThenResettleInOneBatch(t *testing.T) {
	t.Parallel()

	// A fast walk can unsettle and re-settle a collection inside one flush
	// window. The withdrawal must still lead the entries and the re-earned
	// settlement must still trail them, so any torn append leaves either no
	// new entries or entries guarded by a durable false.
	sh := newShard(t.TempDir())

	sh.entries["runs/r4/run.json"] = &Entry{Status: StatusDone}
	sh.dirtyEntries["runs/r4/run.json"] = struct{}{}
	sh.settled["runs"] = true
	sh.dirtySettled["runs"] = struct{}{}
	sh.dirtyUnsettled["runs"] = struct{}{}

	d := sh.drainDirty()

	require.Len(t, d.recs, 3)
	assert.Equal(t, walSettled, d.recs[0].Kind)
	assert.False(t, d.recs[0].Settled, "the withdrawal leads the entries")
	assert.Equal(t, walEntry, d.recs[1].Kind)
	assert.Equal(t, walSettled, d.recs[2].Kind)
	assert.True(t, d.recs[2].Settled, "the re-earned settlement trails the entries")
}

func TestShardDrainRecordsReplayToSameState(t *testing.T) {
	t.Parallel()

	src := newShard(t.TempDir())
	dirtyEverything(t, src)

	d := src.drainDirty()

	// Replaying the drained records into a fresh shard reconstructs the same
	// state the drain captured: the record kinds and the apply switch stay in
	// step for every category the shard tracks.
	dst := newShard(t.TempDir())
	for i := range d.recs {
		require.NoError(t, dst.applyRecord(&d.recs[i]))
	}

	assert.Equal(t, src.entries, dst.entries)
	assert.Equal(t, src.watermarks, dst.watermarks)
	assert.Equal(t, src.completed, dst.completed)
	assert.Equal(t, src.settled, dst.settled)
	assert.Equal(t, src.runCount, dst.runCount)
}
