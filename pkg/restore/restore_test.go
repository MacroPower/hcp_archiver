package restore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote/remotetest"
	"go.jacobcolvin.com/hcp_archiver/pkg/restore"
)

const (
	pullPrefix = "hcp"
	pullOrg    = "acme"
	pullWs     = "projects/p/workspaces/w"

	orgContent = `{"data":{"id":"org-1","type":"organizations","attributes":{"name":"acme"}}}`

	orgSnapshotOld = `{"version":2,"lastRunAt":"2026-08-20T10:00:00Z","runCount":3}`
	orgSnapshotNew = `{"version":2,"lastRunAt":"2026-08-24T10:00:00Z","runCount":4}`
)

// warmSet is the restorable fixture content, keyed by archive-relative path.
func warmSet() map[string]string {
	return map[string]string{
		"org.json":                                          orgContent,
		"projects/p/.identity.json":                         `{"name":"p"}`,
		pullWs + "/workspace.json":                          `{"data":{"id":"ws-1"}}`,
		pullWs + "/runs.ndjson":                             "{\"id\":\"r1\"}\n{\"id\":\"r2\"}\n",
		pullWs + "/workspace.history.ndjson":                "{\"v\":1}\n",
		pullWs + "/bundles/logs.gen0001.zip.sidecar.ndjson": "{\"name\":\"m\"}\n",
		pullWs + "/.ledger/snapshot.json":                   `{"version":2}`,
		".ledger/snapshot.json":                             orgSnapshotNew,
	}
}

// excludedSet is mirror content a restore must never materialize, keyed by
// archive-relative path.
func excludedSet() map[string]string {
	return map[string]string{
		".ledger/log.ndjson":                 "{\"resurrected\":true}\n",
		".ledger/lock":                       "",
		pullWs + "/bundles/logs.gen0001.zip": "zip bytes",
		"config-versions/cv-1.tar.gz":        "tarball bytes",
		pullWs + "/.atomicfile-999.tmp":      "partial",
		".remote.json":                       `{"url":"s3://elsewhere","version":1}`,
	}
}

// pullFixture is a restorer over an in-memory mirror and a fresh org root.
type pullFixture struct {
	r       *restore.Restorer
	client  *remote.Client
	fake    *remotetest.Fake
	cfg     remote.Config
	orgRoot string
}

// newPullFixture builds a [pullFixture] whose mirror holds the warm set, the
// excluded set, and one hostile key.
func newPullFixture(t *testing.T) pullFixture {
	t.Helper()

	cfg := remote.Config{Prefix: pullPrefix}
	fake := remotetest.New()

	client, err := remote.New(t.Context(), cfg,
		remote.WithBucket(fake.Bucket()), remote.WithRetry(0, 0))
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	f := pullFixture{
		r:       restore.NewRestorer(client, cfg),
		client:  client,
		fake:    fake,
		cfg:     cfg,
		orgRoot: filepath.Join(t.TempDir(), pullOrg),
	}

	for rel, content := range warmSet() {
		f.put(t, rel, content)
	}

	for rel, content := range excludedSet() {
		f.put(t, rel, content)
	}

	// A bucket key spelling a traversal must drop from the plan, never join
	// under the root.
	fake.SetObject(pullPrefix+"/"+pullOrg+"/../evil", remotetest.Object{Data: []byte("escape")})

	return f
}

// put seeds one mirrored object through the client, so its digests ride as
// metadata the way a real archive run records them.
func (f pullFixture) put(t *testing.T, rel, content string) {
	t.Helper()

	require.NoError(t, f.client.Put(t.Context(), f.cfg.Key(pullOrg, rel), []byte(content)))
}

// pull plans and executes one restore, requiring the plan to build.
func (f pullFixture) pull(t *testing.T) restore.Summary {
	t.Helper()

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)

	sum, err := f.r.Pull(t.Context(), f.orgRoot, pullOrg, plan)
	require.NoError(t, err)

	return sum
}

// content reads one restored file.
func (f pullFixture) content(t *testing.T, rel string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(f.orgRoot, filepath.FromSlash(rel)))
	require.NoError(t, err)

	return string(data)
}

// exists reports whether rel is on disk under the org root.
func (f pullFixture) exists(t *testing.T, rel string) bool {
	t.Helper()

	_, err := os.Stat(filepath.Join(f.orgRoot, filepath.FromSlash(rel)))
	if err != nil {
		require.ErrorIs(t, err, os.ErrNotExist)

		return false
	}

	return true
}

// writeLocal writes one local file under the org root.
func (f pullFixture) writeLocal(t *testing.T, rel, content string) {
	t.Helper()

	abs := filepath.Join(f.orgRoot, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o700))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))
}

// marker reads the org root's remote marker, requiring one.
func (f pullFixture) marker(t *testing.T) remote.Marker {
	t.Helper()

	marker, ok, err := remote.ReadMarker(f.orgRoot)
	require.NoError(t, err)
	require.True(t, ok, "the org root should carry a marker")

	return marker
}

func TestPullRestoresWarmLayerIntoEmptyRoot(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)

	assert.Equal(t, len(warmSet()), plan.RestoreFiles)
	assert.Zero(t, plan.Skipped)
	assert.Empty(t, plan.Refusals)

	sum, err := f.r.Pull(t.Context(), f.orgRoot, pullOrg, plan)
	require.NoError(t, err)

	assert.Equal(t, len(warmSet()), sum.Restored)
	assert.Zero(t, sum.Failed)
	assert.Zero(t, sum.Refused)

	for rel, content := range warmSet() {
		assert.Equal(t, content, f.content(t, rel), "restored %s should match the mirror", rel)
	}

	marker := f.marker(t)
	assert.False(t, marker.Restoring, "a completed restore settles its marker")
	assert.True(t, marker.Partial,
		"a restored tree does not account for evicted tarball stubs, so it stays partial")
	assert.Equal(t, remote.MarkerVersion, marker.Version)
}

func TestPullNeverMaterializesExcludedFiles(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)

	for _, e := range plan.Entries {
		_, excluded := excludedSet()[e.Rel]
		assert.False(t, excluded, "the plan must not carry %s", e.Rel)
		assert.NotContains(t, e.Rel, "..", "a hostile key must drop from the plan")
	}

	_, err = f.r.Pull(t.Context(), f.orgRoot, pullOrg, plan)
	require.NoError(t, err)

	// The replay log is the load-bearing exclusion: restored beside a newer
	// snapshot it would replay superseded ledger state.
	assert.False(t, f.exists(t, ".ledger/log.ndjson"))
	assert.False(t, f.exists(t, ".ledger/lock"))
	assert.False(t, f.exists(t, pullWs+"/bundles/logs.gen0001.zip"))
	assert.False(t, f.exists(t, "config-versions/cv-1.tar.gz"))
	assert.False(t, f.exists(t, pullWs+"/.atomicfile-999.tmp"))
	assert.False(t, f.exists(t, "evil"))

	// The marker on disk is the one pull wrote, not the mirrored object.
	assert.NotEqual(t, "s3://elsewhere", f.marker(t).URL)
}

func TestPullDigestFailureHoldsEverySnapshotBack(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)

	// One data object whose body does not hash to the digest the mirror
	// records, sized exactly right so only the digest check can catch it.
	good := []byte("expected roll-up line\n")
	bad := []byte("tampered roll-up line\n")
	require.Len(t, bad, len(good))

	sum := sha256.Sum256(good)
	f.fake.SetObject(f.cfg.Key(pullOrg, pullWs+"/runs.ndjson"), remotetest.Object{
		Data:     bad,
		Metadata: map[string]string{"sha256": hex.EncodeToString(sum[:])},
	})

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)

	summary, err := f.r.Pull(t.Context(), f.orgRoot, pullOrg, plan)
	require.NoError(t, err)

	require.Equal(t, 1, summary.Failed, "the poisoned object must fail, not land")
	require.Len(t, summary.Failures, 1)
	assert.Equal(t, pullWs+"/runs.ndjson", summary.Failures[0].Path)
	require.ErrorIs(t, summary.Failures[0].Err, remote.ErrDigestMismatch)

	assert.False(t, f.exists(t, pullWs+"/runs.ndjson"),
		"a failed verification must leave no file")

	// The ordering barrier: no snapshot lands over an unproven data layer,
	// so the tree never holds ledger entries describing absent files.
	assert.False(t, f.exists(t, ".ledger/snapshot.json"))
	assert.False(t, f.exists(t, pullWs+"/.ledger/snapshot.json"))

	assert.True(t, f.marker(t).Restoring,
		"an incomplete restore keeps its marker standing")
}

func TestPullIsIdempotent(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)
	f.pull(t)

	markerBefore, err := os.ReadFile(filepath.Join(f.orgRoot, remote.MarkerName))
	require.NoError(t, err)

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)

	assert.Zero(t, plan.RestoreFiles, "a restored archive plans no work")
	assert.Equal(t, len(warmSet()), plan.Skipped)

	sum, err := f.r.Pull(t.Context(), f.orgRoot, pullOrg, plan)
	require.NoError(t, err)

	assert.Zero(t, sum.Restored)
	assert.Equal(t, len(warmSet()), sum.Skipped)

	markerAfter, err := os.ReadFile(filepath.Join(f.orgRoot, remote.MarkerName))
	require.NoError(t, err)
	assert.Equal(t, markerBefore, markerAfter, "a no-op re-run must not rewrite the settled marker")
}

func TestPullFinalizesLeftoverRestoringMarker(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)
	f.pull(t)

	// The state an interrupt between the last file and the marker rewrite
	// leaves: every file present and verified, the marker still restoring.
	// The re-run's plan changes nothing, but leaving the marker would strand
	// the archive behind the restore-in-progress refusals forever.
	require.NoError(t, os.WriteFile(filepath.Join(f.orgRoot, remote.MarkerName),
		[]byte(`{"url":"","version":2,"partial":true,"restoring":true}`), 0o600))

	sum := f.pull(t)

	assert.Zero(t, sum.Restored)
	assert.False(t, f.marker(t).Restoring, "a proven-whole tree must settle its leftover marker")
}

func TestPullResumesAfterPartialLoss(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)
	f.pull(t)

	require.NoError(t, os.Remove(filepath.Join(f.orgRoot, "org.json")))
	require.NoError(t, os.Remove(filepath.Join(f.orgRoot, filepath.FromSlash(pullWs+"/runs.ndjson"))))

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)

	assert.Equal(t, 2, plan.RestoreFiles, "a resume downloads only what is missing")

	sum, err := f.r.Pull(t.Context(), f.orgRoot, pullOrg, plan)
	require.NoError(t, err)

	assert.Equal(t, 2, sum.Restored)
	assert.JSONEq(t, orgContent, f.content(t, "org.json"))
}

func TestPullReplacesDivergedRollup(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)

	// A local roll-up carrying lines the mirror already holds: appending
	// would duplicate them, so the mirrored copy replaces the file whole.
	f.writeLocal(t, pullWs+"/runs.ndjson", "{\"id\":\"r1\"}\n{\"id\":\"r2\"}\n{\"id\":\"local\"}\n")

	f.pull(t)

	assert.Equal(t, warmSet()[pullWs+"/runs.ndjson"], f.content(t, pullWs+"/runs.ndjson"),
		"a diverged roll-up is replaced, never appended to")
}

func TestPullSnapshotConflicts(t *testing.T) {
	t.Parallel()

	t.Run("a differing non-root snapshot refuses", func(t *testing.T) {
		t.Parallel()

		f := newPullFixture(t)
		f.writeLocal(t, pullWs+"/.ledger/snapshot.json", `{"version":2,"entries":{"x":{}}}`)

		plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
		require.NoError(t, err)

		require.Len(t, plan.Refusals, 1)
		assert.Equal(t, pullWs+"/.ledger/snapshot.json", plan.Refusals[0].Rel)

		sum, err := f.r.Pull(t.Context(), f.orgRoot, pullOrg, plan)
		require.NoError(t, err)

		assert.Equal(t, 1, sum.Refused)
		require.Len(t, sum.Failures, 1)
		assert.Equal(t, pullWs+"/.ledger/snapshot.json", sum.Failures[0].Path,
			"a refusal names its path")
		assert.JSONEq(t, `{"version":2,"entries":{"x":{}}}`, f.content(t, pullWs+"/.ledger/snapshot.json"),
			"a refused snapshot is left untouched")
	})

	t.Run("a newer local org-root snapshot is kept", func(t *testing.T) {
		t.Parallel()

		f := newPullFixture(t)
		f.put(t, ".ledger/snapshot.json", orgSnapshotOld)
		f.writeLocal(t, ".ledger/snapshot.json", orgSnapshotNew)

		sum := f.pull(t)

		assert.Zero(t, sum.Refused)
		assert.JSONEq(t, orgSnapshotNew, f.content(t, ".ledger/snapshot.json"),
			"local run state the mirror never saw must not be overwritten")
	})

	t.Run("a newer mirrored org-root snapshot replaces", func(t *testing.T) {
		t.Parallel()

		f := newPullFixture(t)
		f.writeLocal(t, ".ledger/snapshot.json", orgSnapshotOld)

		sum := f.pull(t)

		assert.Zero(t, sum.Refused)
		assert.JSONEq(t, orgSnapshotNew, f.content(t, ".ledger/snapshot.json"))
	})

	t.Run("an undecodable local org-root snapshot refuses per path", func(t *testing.T) {
		t.Parallel()

		f := newPullFixture(t)
		f.writeLocal(t, ".ledger/snapshot.json", `{not json`)

		plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
		require.NoError(t, err, "one corrupt snapshot must not abort the whole plan")

		require.Len(t, plan.Refusals, 1)
		assert.Equal(t, ".ledger/snapshot.json", plan.Refusals[0].Rel)
		assert.Positive(t, plan.RestoreFiles, "the rest of the set still restores")
	})

	t.Run("an unorderable org-root snapshot refuses", func(t *testing.T) {
		t.Parallel()

		f := newPullFixture(t)

		// Same run metadata, different bytes: nothing orders the copies.
		f.writeLocal(t, ".ledger/snapshot.json",
			`{"version":2,"lastRunAt":"2026-08-24T10:00:00Z","runCount":4,"entries":{"x":{}}}`)

		plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
		require.NoError(t, err)

		require.Len(t, plan.Refusals, 1)
		assert.Equal(t, ".ledger/snapshot.json", plan.Refusals[0].Rel)
	})
}

func TestPullInterruptDefersSnapshotsAndKeepsMarker(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)

	// The interrupt lands mid-Phase-A: the first settled file cancels the
	// context, so the run winds down with data files outstanding.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	interrupted := restore.NewRestorer(f.client, f.cfg,
		restore.WithConcurrency(1),
		restore.WithProgress(func(string, int64, error) { cancel() }),
	)

	_, err = interrupted.Pull(ctx, f.orgRoot, pullOrg, plan)
	require.NoError(t, err)

	assert.False(t, f.exists(t, ".ledger/snapshot.json"),
		"an interrupted run must not land snapshots over an unproven data layer")
	assert.False(t, f.exists(t, pullWs+"/.ledger/snapshot.json"))
	assert.True(t, f.marker(t).Restoring,
		"an interrupted restore keeps its marker standing")

	// The re-run finishes what the interrupt left.
	sum := f.pull(t)

	assert.Zero(t, sum.Failed)
	assert.False(t, f.marker(t).Restoring)

	for rel, content := range warmSet() {
		assert.Equal(t, content, f.content(t, rel), "resumed restore should complete %s", rel)
	}
}

func TestPullRefusesLocalReplayLog(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)
	f.writeLocal(t, ".ledger/log.ndjson", "{\"local\":true}\n")

	_, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.ErrorIs(t, err, restore.ErrLocalReplayLog,
		"a local log where snapshots must land refuses before anything is written")

	assert.False(t, f.exists(t, remote.MarkerName),
		"the preflight refusal must precede the restoring marker")
}

func TestPullAllowsLocalReplayLogWhenNoSnapshotLands(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)
	f.pull(t)

	// A restored archive later archived locally again: the log holds new
	// changes and every mirrored snapshot is already on disk, so a re-run
	// plans no snapshot write and the log is no conflict.
	f.writeLocal(t, ".ledger/log.ndjson", "{\"local\":true}\n")

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)
	assert.Zero(t, plan.RestoreFiles)
}

func TestPullRefusesForeignOrgRoot(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)
	f.writeLocal(t, "org.json",
		`{"data":{"id":"org-2","type":"organizations","attributes":{"name":"globex"}}}`)

	_, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.ErrorIs(t, err, restore.ErrOrgMismatch)
}
