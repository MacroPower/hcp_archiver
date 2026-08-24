package restore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote/remotetest"
	"go.jacobcolvin.com/hcp_archiver/pkg/restore"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
)

const (
	pullPrefix = "hcp"
	pullOrg    = "acme"
	pullWs     = "projects/p/workspaces/w"

	orgContent = `{"data":{"id":"org-1","type":"organizations","attributes":{"name":"acme"}}}`

	orgSnapshotOld = `{"version":2,"lastRunAt":"2026-08-20T10:00:00Z","runCount":3}`
	orgSnapshotNew = `{"version":2,"lastRunAt":"2026-08-24T10:00:00Z","runCount":4}`

	tarballRel     = "config-versions/cv-1.tar.gz"
	tarballContent = "tarball bytes"
	zipRel         = pullWs + "/bundles/logs.gen0001.zip"
	stubRel        = tarballRel + ".remote.json"
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
// archive-relative path: the evicted surfaces, which stay remote, plus junk
// keys a healthy sweep would never upload.
func excludedSet() map[string]string {
	return map[string]string{
		".ledger/log.ndjson":            "{\"resurrected\":true}\n",
		".ledger/lock":                  "",
		zipRel:                          "zip bytes",
		tarballRel:                      tarballContent,
		pullWs + "/.atomicfile-999.tmp": "partial",
		".remote.json":                  `{"url":"s3://elsewhere","version":1}`,
	}
}

// junkLeftovers is the accounting the junk fixture's mirror forces: the keys
// nothing in a restored tree accounts for, sorted the way a plan reports
// them (the hostile key stays raw, having no relative form).
func junkLeftovers() []string {
	return []string{
		".ledger/lock",
		".ledger/log.ndjson",
		pullPrefix + "/" + pullOrg + "/../evil",
		pullWs + "/.atomicfile-999.tmp",
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

// newBareFixture builds a [pullFixture] over an empty in-memory mirror. The
// URL is nominal (the injected bucket serves the bytes) but the markers the
// restore writes record it, as every real configuration's would.
func newBareFixture(t *testing.T) pullFixture {
	t.Helper()

	cfg := remote.Config{URL: "mem://archive", Prefix: pullPrefix}
	fake := remotetest.New()

	client, err := remote.New(t.Context(), cfg,
		remote.WithBucket(fake.Bucket()), remote.WithRetry(0, 0))
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	return pullFixture{
		r:       restore.NewRestorer(client, cfg),
		client:  client,
		fake:    fake,
		cfg:     cfg,
		orgRoot: filepath.Join(t.TempDir(), pullOrg),
	}
}

// newPullFixture builds the junk fixture: the warm set, the excluded set
// (junk keys included), and one hostile key. Its mirror can never settle a
// complete marker.
func newPullFixture(t *testing.T) pullFixture {
	t.Helper()

	f := newBareFixture(t)

	for rel, content := range warmSet() {
		f.put(t, rel, content)
	}

	for rel, content := range excludedSet() {
		f.put(t, rel, content)
	}

	// A bucket key spelling a traversal must drop from the plan, never join
	// under the root.
	f.fake.SetObject(pullPrefix+"/"+pullOrg+"/../evil", remotetest.Object{Data: []byte("escape")})

	return f
}

// newCleanPullFixture builds the healthy fixture: the warm set plus only the
// two evicted surfaces, the mirror a clean archiver run leaves behind, whose
// restore settles a complete marker.
func newCleanPullFixture(t *testing.T) pullFixture {
	t.Helper()

	f := newBareFixture(t)

	for rel, content := range warmSet() {
		f.put(t, rel, content)
	}

	f.put(t, zipRel, "zip bytes")
	f.put(t, tarballRel, tarballContent)

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

// readStub reads and parses the tarball's local eviction stub.
func (f pullFixture) readStub(t *testing.T) store.RemoteStub {
	t.Helper()

	var stub store.RemoteStub

	require.NoError(t, json.Unmarshal([]byte(f.content(t, stubRel)), &stub))

	return stub
}

// requireCanonicalStub asserts the tarball's stub records exactly what the
// mirror holds.
func (f pullFixture) requireCanonicalStub(t *testing.T) {
	t.Helper()

	stub := f.readStub(t)

	sum := sha256.Sum256([]byte(tarballContent))
	assert.Equal(t, store.RemoteStub{
		Version: store.RemoteStubVersion,
		Size:    int64(len(tarballContent)),
		SHA256:  hex.EncodeToString(sum[:]),
	}, stub)
}

func TestPullRestoresWarmLayerIntoEmptyRoot(t *testing.T) {
	t.Parallel()

	f := newCleanPullFixture(t)

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)

	assert.Equal(t, len(warmSet()), plan.RestoreFiles)
	assert.Zero(t, plan.Skipped)
	assert.Empty(t, plan.Refusals)
	assert.Empty(t, plan.Leftovers)
	assert.Equal(t, []restore.StubEntry{{Rel: tarballRel}}, plan.Stubs)

	sum, err := f.r.Pull(t.Context(), f.orgRoot, pullOrg, plan)
	require.NoError(t, err)

	assert.Equal(t, len(warmSet()), sum.Restored)
	assert.Zero(t, sum.Failed)
	assert.Zero(t, sum.Refused)
	assert.Equal(t, 1, sum.Stubs)
	assert.Zero(t, sum.StubsFailed)
	assert.True(t, sum.Complete)
	assert.Empty(t, sum.Leftovers)

	for rel, content := range warmSet() {
		assert.Equal(t, content, f.content(t, rel), "restored %s should match the mirror", rel)
	}

	f.requireCanonicalStub(t)

	marker := f.marker(t)
	assert.False(t, marker.Restoring, "a completed restore settles its marker")
	assert.False(t, marker.Partial,
		"a restore that accounts for every mirrored key settles the marker complete")
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
	assert.False(t, f.exists(t, zipRel))
	assert.False(t, f.exists(t, tarballRel))
	assert.False(t, f.exists(t, pullWs+"/.atomicfile-999.tmp"))
	assert.False(t, f.exists(t, "evil"))

	// The tarball's bytes stay remote, but its eviction stub lands, and the
	// junk keys keep the marker partial.
	f.requireCanonicalStub(t)
	assert.True(t, f.marker(t).Partial,
		"a mirror holding unaccounted keys must not settle a complete marker")

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
	// so the tree never holds ledger entries describing absent files, and
	// the stub phase is gated on the same proof.
	assert.False(t, f.exists(t, ".ledger/snapshot.json"))
	assert.False(t, f.exists(t, pullWs+"/.ledger/snapshot.json"))
	assert.False(t, f.exists(t, stubRel))

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
	assert.False(t, sum.Complete, "the junk keys keep the marker partial on every run")

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
	assert.True(t, f.marker(t).Partial, "the junk keys keep the settled marker partial")
}

func TestPullSettlesLeftoverRestoringMarkerComplete(t *testing.T) {
	t.Parallel()

	f := newCleanPullFixture(t)
	f.pull(t)

	require.NoError(t, os.WriteFile(filepath.Join(f.orgRoot, remote.MarkerName),
		[]byte(`{"url":"","version":2,"partial":true,"restoring":true}`), 0o600))

	sum := f.pull(t)

	assert.Zero(t, sum.Restored)
	assert.True(t, sum.Complete)

	marker := f.marker(t)
	assert.False(t, marker.Restoring)
	assert.False(t, marker.Partial,
		"a proven-whole tree over a clean mirror settles its leftover marker complete")
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

	// The re-run finishes what the interrupt left; the junk keys keep the
	// settled marker partial.
	sum := f.pull(t)

	assert.Zero(t, sum.Failed)
	assert.False(t, f.marker(t).Restoring)
	assert.True(t, f.marker(t).Partial)

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

func TestPullStaysPartialAndNamesLeftovers(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)

	sum := f.pull(t)

	assert.Zero(t, sum.Failed)
	assert.False(t, sum.Complete)
	assert.Equal(t, junkLeftovers(), sum.Leftovers,
		"every unaccounted mirror key is named, the hostile one by its raw key")
	assert.True(t, f.marker(t).Partial)
}

func TestPlanCarriesStubAndLeftoverAccounting(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)

	assert.Equal(t, []restore.StubEntry{{Rel: tarballRel}}, plan.Stubs)
	assert.Equal(t, junkLeftovers(), plan.Leftovers)
}

// demoteMarkerPartial rewrites the org marker as a bare partial one, the
// shape an earlier build's pull left behind.
func demoteMarkerPartial(t *testing.T, orgRoot string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(orgRoot, remote.MarkerName),
		[]byte(`{"url":"","version":1,"partial":true}`), 0o600))
}

func TestPullPromotesOldPullTree(t *testing.T) {
	t.Parallel()

	f := newCleanPullFixture(t)
	f.pull(t)

	// The tree an earlier build's pull restored: every file present, no
	// stub, a partial marker.
	require.NoError(t, os.Remove(filepath.Join(f.orgRoot, filepath.FromSlash(stubRel))))
	demoteMarkerPartial(t, f.orgRoot)

	sum := f.pull(t)

	assert.Zero(t, sum.Restored)
	assert.Equal(t, 1, sum.Stubs)
	assert.True(t, sum.Complete)
	f.requireCanonicalStub(t)
	assert.False(t, f.marker(t).Partial, "a zero-transfer run still backfills and promotes")
}

func TestPullRefusesContradictingStub(t *testing.T) {
	t.Parallel()

	f := newCleanPullFixture(t)
	f.pull(t)

	// A valid stub recording a size the mirror contradicts: the stub is the
	// only independent record of a mirror-side change, so it is reported,
	// never silently replaced.
	planted := `{"sha256":"deadbeef","size":999,"version":1}`
	f.writeLocal(t, stubRel, planted)
	demoteMarkerPartial(t, f.orgRoot)

	sum := f.pull(t)

	assert.Equal(t, 1, sum.StubsFailed)
	assert.Zero(t, sum.Failed)
	assert.False(t, sum.Complete)
	assert.Equal(t, planted, f.content(t, stubRel), "a contradicting stub is left standing")
	assert.True(t, f.marker(t).Partial)
}

func TestPullRepairsCorruptStub(t *testing.T) {
	t.Parallel()

	f := newCleanPullFixture(t)
	f.pull(t)

	f.writeLocal(t, stubRel, `{not json`)
	demoteMarkerPartial(t, f.orgRoot)

	sum := f.pull(t)

	assert.Equal(t, 1, sum.Stubs)
	assert.Zero(t, sum.StubsFailed)
	assert.True(t, sum.Complete)
	f.requireCanonicalStub(t)
}

func TestPullUpgradesDigestlessStub(t *testing.T) {
	t.Parallel()

	t.Run("a digestless stub gains the mirror's digest", func(t *testing.T) {
		t.Parallel()

		f := newCleanPullFixture(t)
		f.pull(t)

		f.writeLocal(t, stubRel, fmt.Sprintf(`{"size":%d,"version":1}`, len(tarballContent)))
		demoteMarkerPartial(t, f.orgRoot)

		sum := f.pull(t)

		assert.Zero(t, sum.StubsFailed)
		assert.True(t, sum.Complete)
		f.requireCanonicalStub(t)
	})

	t.Run("a digest-bearing stub survives a digestless mirror", func(t *testing.T) {
		t.Parallel()

		f := newCleanPullFixture(t)
		f.pull(t)

		// The mirror's record loses its metadata (a foreign rewrite, a
		// stripped copy); the stub in place is the stronger record and the
		// size still matches, so nothing is wrong.
		f.fake.SetObject(f.cfg.Key(pullOrg, tarballRel),
			remotetest.Object{Data: []byte(tarballContent)})
		demoteMarkerPartial(t, f.orgRoot)

		sum := f.pull(t)

		assert.Zero(t, sum.StubsFailed)
		assert.True(t, sum.Complete)
		f.requireCanonicalStub(t)
	})
}

func TestPullNeverClobbersNewerStub(t *testing.T) {
	t.Parallel()

	f := newCleanPullFixture(t)
	f.pull(t)

	planted := fmt.Sprintf(`{"size":%d,"version":99}`, len(tarballContent))
	f.writeLocal(t, stubRel, planted)
	demoteMarkerPartial(t, f.orgRoot)

	sum := f.pull(t)

	assert.Equal(t, 1, sum.StubsFailed)
	assert.False(t, sum.Complete)
	assert.Equal(t, planted, f.content(t, stubRel),
		"a stub a newer build wrote is never overwritten")
	assert.True(t, f.marker(t).Partial)
}

func TestPullDigestlessTarballStubPromotes(t *testing.T) {
	t.Parallel()

	f := newCleanPullFixture(t)

	// A tarball whose upload recorded no digest at all: the stub is written
	// digestless, the same shape the viewer's own synthesis produces.
	f.fake.SetObject(f.cfg.Key(pullOrg, tarballRel),
		remotetest.Object{Data: []byte(tarballContent)})

	sum := f.pull(t)

	assert.True(t, sum.Complete)

	stub := f.readStub(t)
	assert.Empty(t, stub.SHA256)
	assert.Equal(t, int64(len(tarballContent)), stub.Size)
}

func TestPullStubHeadFailureBlocksPromotionOnly(t *testing.T) {
	t.Parallel()

	f := newCleanPullFixture(t)

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)

	// The tarball vanishes between plan and execute: the stub cannot be
	// ensured, which is a read-model gap, never a restore failure.
	_, err = f.client.Delete(t.Context(), []string{f.cfg.Key(pullOrg, tarballRel)})
	require.NoError(t, err)

	sum, err := f.r.Pull(t.Context(), f.orgRoot, pullOrg, plan)
	require.NoError(t, err)

	assert.Zero(t, sum.Failed)
	assert.Equal(t, 1, sum.StubsFailed)
	assert.False(t, sum.Complete)
	assert.True(t, f.marker(t).Partial)
}

func TestPullZipWithoutSidecarStaysPartial(t *testing.T) {
	t.Parallel()

	f := newBareFixture(t)

	sidecarRel := zipRel + ".sidecar.ndjson"

	for rel, content := range warmSet() {
		if rel == sidecarRel {
			continue
		}

		f.put(t, rel, content)
	}

	f.put(t, zipRel, "zip bytes")

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)

	assert.Equal(t, []string{zipRel}, plan.Leftovers,
		"a zip with no sidecar anywhere has no local trace to account for it")

	sum, err := f.r.Pull(t.Context(), f.orgRoot, pullOrg, plan)
	require.NoError(t, err)

	assert.False(t, sum.Complete)
	assert.True(t, f.marker(t).Partial)
}

func TestPullNeverDemotesCompleteMarker(t *testing.T) {
	t.Parallel()

	f := newCleanPullFixture(t)
	f.pull(t)

	// The mirror gains a junk key and the tree loses a file: the re-run
	// restores the file, and the proven-complete marker is not flipped over
	// junk the next archiver run will prune.
	f.put(t, ".ledger/log.ndjson", "{\"seeded\":true}\n")
	require.NoError(t, os.Remove(filepath.Join(f.orgRoot, "org.json")))

	sum := f.pull(t)

	assert.Equal(t, 1, sum.Restored)
	assert.True(t, sum.Complete)
	assert.Equal(t, []string{".ledger/log.ndjson"}, sum.Leftovers,
		"the junk key is still named, it just cannot demote a proven tree")
	assert.False(t, f.marker(t).Partial)
	assert.False(t, f.exists(t, ".ledger/log.ndjson"))
}

func TestPullRepairsLostStubUnderCompleteMarker(t *testing.T) {
	t.Parallel()

	f := newCleanPullFixture(t)
	f.pull(t)

	require.NoError(t, os.Remove(filepath.Join(f.orgRoot, filepath.FromSlash(stubRel))))

	// A complete marker's reads have no mirror fallback, so the lost stub
	// would strand its tarball; the zero-transfer re-run repairs exactly it.
	sum := f.pull(t)

	assert.Zero(t, sum.Restored)
	assert.Equal(t, 1, sum.Stubs)
	assert.True(t, sum.Complete)
	f.requireCanonicalStub(t)
	assert.False(t, f.marker(t).Partial)
}

func TestPullSettleWritesNothingWithoutMarker(t *testing.T) {
	t.Parallel()

	f := newBareFixture(t)

	sum := f.pull(t)

	assert.Zero(t, sum.Restored)
	assert.False(t, sum.Complete)
	assert.False(t, f.exists(t, remote.MarkerName),
		"a tree that never claimed the mirror stands in gains no marker from a no-op run")
}

func TestPullStubBytesMatchSweep(t *testing.T) {
	t.Parallel()

	f := newCleanPullFixture(t)
	f.pull(t)

	// The sweep skips a rewrite when the bytes already match, so pull's stub
	// must serialize exactly as the sweep's own write does.
	sweepDir := t.TempDir()

	sum := sha256.Sum256([]byte(tarballContent))
	_, err := store.New(sweepDir).WriteJSON(stubRel, store.RemoteStub{
		Version: store.RemoteStubVersion,
		Size:    int64(len(tarballContent)),
		SHA256:  hex.EncodeToString(sum[:]),
	})
	require.NoError(t, err)

	want, err := os.ReadFile(filepath.Join(sweepDir, filepath.FromSlash(stubRel)))
	require.NoError(t, err)

	assert.Equal(t, string(want), f.content(t, stubRel))
}

func TestPullLocalTarballNeedsNoStub(t *testing.T) {
	t.Parallel()

	f := newCleanPullFixture(t)

	// The tarball's own bytes sit locally (not yet evicted): the file itself
	// accounts for the key, and a stub beside a live tarball is the
	// eviction's crash shape, not a state to create.
	f.writeLocal(t, tarballRel, tarballContent)

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)

	assert.Empty(t, plan.Stubs)

	sum, err := f.r.Pull(t.Context(), f.orgRoot, pullOrg, plan)
	require.NoError(t, err)

	assert.True(t, sum.Complete)
	assert.False(t, f.exists(t, stubRel))
}

func TestPullInterruptedStubPhaseResumes(t *testing.T) {
	t.Parallel()

	f := newCleanPullFixture(t)

	plan, err := f.r.Plan(t.Context(), f.orgRoot, pullOrg)
	require.NoError(t, err)

	// The cancellation lands as the last snapshot settles, so the stub
	// phase and the settlement both see a dead context.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	interrupted := restore.NewRestorer(f.client, f.cfg,
		restore.WithConcurrency(1),
		restore.WithProgress(func(relPath string, _ int64, _ error) {
			if relPath == pullWs+"/.ledger/snapshot.json" {
				cancel()
			}
		}),
	)

	_, err = interrupted.Pull(ctx, f.orgRoot, pullOrg, plan)
	require.NoError(t, err)

	assert.False(t, f.exists(t, stubRel), "an interrupted run lands no stub")
	assert.True(t, f.marker(t).Restoring)

	sum := f.pull(t)

	assert.True(t, sum.Complete)
	f.requireCanonicalStub(t)
	assert.False(t, f.marker(t).Partial)
}

// sinkRecord is a [restore.ProgressSink] recording every call: the phases in
// the order they were named, the totals in the order they were seeded, the
// advances tallied under the phase current when each landed, and the failed
// transfers.
type sinkRecord struct {
	advances map[string]int
	current  string
	phases   []string
	totals   []int
	errored  int
	mu       sync.Mutex
}

func (s *sinkRecord) SetPhase(phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.phases = append(s.phases, phase)
	s.current = phase
}

func (s *sinkRecord) SetTotal(total int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.totals = append(s.totals, total)
}

func (s *sinkRecord) Advance(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.advances == nil {
		s.advances = map[string]int{}
	}

	s.advances[s.current] += n
}

func (s *sinkRecord) Errored(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.errored += n
}

func TestPullReportsPhasesThroughSink(t *testing.T) {
	t.Parallel()

	sink := &sinkRecord{}

	f := newCleanPullFixture(t)
	f.r = restore.NewRestorer(f.client, f.cfg, restore.WithProgressSink(sink))

	sum := f.pull(t)
	require.True(t, sum.Complete)

	assert.Equal(t, []string{restore.PhaseRestore, restore.PhaseStubs, restore.PhaseSettle},
		sink.phases, "the phases run restore, then stubs, then settle")
	assert.Equal(t, []int{len(warmSet()), 1, 0}, sink.totals,
		"restore counts every planned file, stubs count the tarball's, settle is indeterminate")
	assert.Equal(t, map[string]int{
		restore.PhaseRestore: len(warmSet()),
		restore.PhaseStubs:   1,
	}, sink.advances, "every unit settles under the phase that owns it")
	assert.Zero(t, sink.errored, "a clean restore errors nothing")
}

func TestPullSettleOnlyReportsPhasesThroughSink(t *testing.T) {
	t.Parallel()

	f := newPullFixture(t)
	f.pull(t)

	// A converged re-run transfers nothing, but the stub verification and
	// the marker settlement still report, so a settle-only pass over
	// thousands of stubs is not a silent hang.
	sink := &sinkRecord{}
	f.r = restore.NewRestorer(f.client, f.cfg, restore.WithProgressSink(sink))

	f.pull(t)

	assert.Equal(t, []string{restore.PhaseStubs, restore.PhaseSettle}, sink.phases)
	assert.Equal(t, map[string]int{restore.PhaseStubs: 1}, sink.advances)
}
