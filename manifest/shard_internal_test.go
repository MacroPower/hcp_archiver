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
	sh.runDirty = true
	sh.runCount = 3
}

func TestShardDrainRestoreRoundTrip(t *testing.T) {
	t.Parallel()

	sh := newShard(t.TempDir())
	dirtyEverything(t, sh)

	d := sh.drainDirty()

	assert.False(t, sh.hasDirty(), "a drain leaves the shard clean")
	require.Len(t, d.recs, 5, "one record per dirty item, plus the run record")

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
	assert.Equal(t, map[string]struct{}{"s1": {}}, sh.dirtySettled)
	assert.True(t, sh.runDirty)

	// A second drain after the restore re-emits the full delta, so a retried
	// flush appends everything the failed one attempted.
	d2 := sh.drainDirty()
	assert.Len(t, d2.recs, 6, "the restored delta plus the re-dirtied entry")
	assert.False(t, sh.hasDirty())
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
