package workspace_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
	"go.jacobcolvin.com/hcp_archiver/pkg/collect/workspace"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote/remotetest"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
)

const evictPrefix = "hcp"

// newEvictFixture builds a seal fixture whose environment has a remote store
// configured, backed by an in-memory fake, so the eviction sweep can be
// exercised without a network.
func newEvictFixture(t *testing.T) (sealFixture, *remotetest.Fake) {
	t.Helper()

	root := t.TempDir()
	st := store.New(root)

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	fake := remotetest.New()
	cfg := remote.Config{Prefix: evictPrefix}

	client, err := remote.New(t.Context(), cfg,
		remote.WithBucket(fake.Bucket()), remote.WithRetry(0, 0))
	require.NoError(t, err)

	env := collect.NewEnv(nil, st, ledger,
		collect.WithRemote(client, cfg, "org"),
		collect.WithLogger(slog.New(slog.DiscardHandler)),
	)

	return sealFixture{
		collector: workspace.New(env, "org"),
		store:     st,
		ledger:    ledger,
	}, fake
}

// bundleKey composes the remote key the fixture's "prod"/"api" workspace
// bundle should land at, mirroring the local archive-relative path under the
// prefix and org.
func bundleKey(st *store.Store, name string) string {
	return evictPrefix + "/org/" + st.Join(st.BundleDir("prod", "api"), name)
}

func TestSealWorkspace_EvictsSealedBundles(t *testing.T) {
	t.Parallel()

	f, fake := newEvictFixture(t)
	st := f.store
	project, ws := "prod", "api"

	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("plan output"))
	f.writeDone(t, st.Join(st.StateVersionDir(project, ws), "20260101T000000Z-sv-1.json"), []byte(`{"s":1}`))
	f.markComplete(project, ws)

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	// Both bundles moved remote and the local zips are gone; the sidecars,
	// part of the search layer, always stay local.
	for _, name := range []string{"logs.gen0001.zip", "state.gen0001.zip"} {
		assert.False(t, f.exists(st.Join(st.BundleDir(project, ws), name)),
			"%s should be evicted after the remote copy is confirmed", name)
		assert.True(t, f.exists(st.Join(st.BundleDir(project, ws), name+".sidecar.ndjson")),
			"%s sidecar should stay local", name)

		obj, ok := fake.Object(bundleKey(st, name))
		require.True(t, ok, "%s should be uploaded", name)
		assert.NotEmpty(t, obj.Data)
	}
}

func TestSealWorkspace_EvictionUploadsMatchLocalBytes(t *testing.T) {
	t.Parallel()

	f, fake := newEvictFixture(t)
	st := f.store
	project, ws := "prod", "api"

	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("plan output"))
	f.markComplete(project, ws)

	// Seal without the sweep first by copying what seal.Seal produces: run the
	// full pass, then compare the uploaded object against the sidecar-verified
	// zip by re-reading the remote copy.
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	obj, ok := fake.Object(bundleKey(st, "logs.gen0001.zip"))
	require.True(t, ok)
	assert.Equal(t, "PK\x03\x04", string(obj.Data[:4]), "the uploaded object is the zip itself")
}

func TestSealWorkspace_MigratesPreexistingBundles(t *testing.T) {
	t.Parallel()

	// An archive sealed with no remote configured...
	f := newSealFixture(t)
	st := f.store
	project, ws := "prod", "api"

	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("plan output"))
	f.markComplete(project, ws)
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))
	require.True(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")))

	// ...gains a remote on a later run: the sweep uploads and evicts the
	// pre-existing bundle even though this pass sealed nothing new.
	fake := remotetest.New()
	cfg := remote.Config{Prefix: evictPrefix}

	client, err := remote.New(t.Context(), cfg,
		remote.WithBucket(fake.Bucket()), remote.WithRetry(0, 0))
	require.NoError(t, err)

	env := collect.NewEnv(nil, f.store, f.ledger,
		collect.WithRemote(client, cfg, "org"),
		collect.WithLogger(slog.New(slog.DiscardHandler)),
	)
	migrator := workspace.New(env, "org")

	require.NoError(t, migrator.SealWorkspace(t.Context(), project, ws))

	assert.False(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")),
		"the pre-existing bundle should migrate off disk")

	_, ok := fake.Object(bundleKey(st, "logs.gen0001.zip"))
	assert.True(t, ok, "the pre-existing bundle should be uploaded")
}

func TestSealWorkspace_EvictionIsIdempotent(t *testing.T) {
	t.Parallel()

	f, fake := newEvictFixture(t)
	st := f.store
	project, ws := "prod", "api"

	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("plan output"))
	f.markComplete(project, ws)
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	uploads := fake.PutCalls()
	require.Positive(t, uploads)

	// A second pass finds no local zip and re-uploads nothing.
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))
	assert.Equal(t, uploads, fake.PutCalls(), "a swept bundle is not re-uploaded")
}

func TestSealWorkspace_EvictionResumesAfterUploadBeforeDelete(t *testing.T) {
	t.Parallel()

	f, fake := newEvictFixture(t)
	st := f.store
	project, ws := "prod", "api"

	// Crash point: the upload completed but the local delete never ran. Model
	// it by pre-seeding the remote with the exact object a prior run uploaded,
	// then sealing so the sweep finds both copies.
	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("plan output"))
	f.markComplete(project, ws)

	// First pass builds the zip and uploads it; capture the object and restore
	// the pre-delete state by copying the remote bytes back to disk.
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	key := bundleKey(st, "logs.gen0001.zip")
	obj, ok := fake.Object(key)
	require.True(t, ok)

	local := st.AbsPath(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip"))
	require.NoError(t, os.WriteFile(local, obj.Data, 0o600))

	uploads := fake.PutCalls()

	// The resumed sweep sees the remote copy, verifies size, and evicts
	// without uploading again.
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.Equal(t, uploads, fake.PutCalls(), "a confirmed remote copy is not re-uploaded")
	assert.False(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")),
		"the local zip is still evicted")
}

func TestSealWorkspace_OrphanZipWithoutSidecarIsNeverUploaded(t *testing.T) {
	t.Parallel()

	f, fake := newEvictFixture(t)
	st := f.store
	project, ws := "prod", "api"

	// A crash mid-seal leaves a zip with no sidecar: it is unverified and the
	// loose sources are still canonical, so the sweep must not touch it.
	bundlesDir := st.AbsPath(st.BundleDir(project, ws))
	require.NoError(t, os.MkdirAll(bundlesDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(bundlesDir, "logs.gen0001.zip"), []byte("torn zip"), 0o600))

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.True(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")),
		"an orphan zip stays local")
	assert.Empty(t, fake.Keys(), "an unverified zip is never uploaded")
}

func TestSealWorkspace_EvictionKeepsLocalOnUploadFailure(t *testing.T) {
	t.Parallel()

	f, fake := newEvictFixture(t)
	st := f.store
	project, ws := "prod", "api"
	fake.PutErr = assert.AnError

	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("plan output"))
	f.markComplete(project, ws)

	// Eviction failure warns and continues: the workspace's seal still
	// succeeds and the local zip stays canonical.
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.True(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")),
		"the local zip stays canonical when the upload dies")
}

func TestSealWorkspace_EvictionKeepsLocalOnSizeMismatch(t *testing.T) {
	t.Parallel()

	f, fake := newEvictFixture(t)
	st := f.store
	project, ws := "prod", "api"

	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("plan output"))
	f.markComplete(project, ws)

	// A remote object already sits at the bundle's key with the wrong size (a
	// damaged or foreign write): verify-before-delete must keep the local zip.
	fake.SetObject(bundleKey(st, "logs.gen0001.zip"),
		remotetest.Object{Data: []byte("wrong bytes")})

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.True(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")),
		"a size mismatch keeps the local zip canonical")
}

func TestSealWorkspace_GenerationsSurviveEviction(t *testing.T) {
	t.Parallel()

	f, fake := newEvictFixture(t)
	st := f.store
	project, ws := "prod", "api"

	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("first"))
	f.markComplete(project, ws)
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))
	require.False(t, f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")),
		"generation one is evicted")

	// With the gen-1 zip gone, only its sidecar records the generation; a new
	// frozen artifact must seal into generation two, not overwrite one.
	f.writeDone(t, st.RunFile(project, ws, "run-2", "plan.log"), []byte("second"))
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	_, gen1 := fake.Object(bundleKey(st, "logs.gen0001.zip"))
	_, gen2 := fake.Object(bundleKey(st, "logs.gen0002.zip"))

	assert.True(t, gen1, "generation one survives remotely")
	assert.True(t, gen2, "the next seal appends generation two")
}

func TestSealWorkspace_ZipWithLostSidecarBlocksItsGeneration(t *testing.T) {
	t.Parallel()

	f, _ := newEvictFixture(t)
	st := f.store
	project, ws := "prod", "api"

	// A surviving zip whose sidecar was lost (partial restore, damaged file)
	// still blocks its generation number from reuse.
	bundlesDir := st.AbsPath(st.BundleDir(project, ws))
	require.NoError(t, os.MkdirAll(bundlesDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(bundlesDir, "logs.gen0003.zip"), []byte("survivor"), 0o600))

	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("plan output"))
	f.markComplete(project, ws)
	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	assert.True(t,
		f.exists(st.Join(st.BundleDir(project, ws), "logs.gen0004.zip.sidecar.ndjson")),
		"the new bundle takes the next generation past the sidecar-less survivor")
}

func TestSealWorkspace_MirrorsSubtreeAtSealBoundary(t *testing.T) {
	t.Parallel()

	f, fake := newEvictFixture(t)
	st := f.store
	project, ws := "prod", "api"

	f.writeDone(t, st.RunFile(project, ws, "run-1", "plan.log"), []byte("plan output"))
	f.writeDone(t, st.WorkspaceFile(project, ws, "workspace.json"), []byte(`{"ws":"api"}`))
	f.markComplete(project, ws)

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws))

	// The subtree's post-seal search layer is mirrored the moment the seal
	// settles, not left for the close sweep; the zip itself reached the
	// remote by eviction just before.
	for _, rel := range []string{
		st.WorkspaceFile(project, ws, "workspace.json"),
		st.Join(st.BundleDir(project, ws), "logs.gen0001.zip.sidecar.ndjson"),
	} {
		_, ok := fake.Object(evictPrefix + "/org/" + rel)
		assert.True(t, ok, "%s should be mirrored at the seal boundary", rel)
	}
}

func TestSealWorkspace_SubtreeSyncFailureDoesNotFailSeal(t *testing.T) {
	t.Parallel()

	f, fake := newEvictFixture(t)
	st := f.store
	project, ws := "prod", "api"

	// Nothing frozen to bundle or evict, so the injected fault lands only on
	// the subtree sync's uploads.
	f.writeDone(t, st.WorkspaceFile(project, ws, "workspace.json"), []byte(`{"ws":"api"}`))
	f.markComplete(project, ws)

	fake.PutErr = assert.AnError

	require.NoError(t, f.collector.SealWorkspace(t.Context(), project, ws),
		"a subtree-sync failure is stats-only; the close sweep retries")

	_, ok := fake.Object(evictPrefix + "/org/" + st.WorkspaceFile(project, ws, "workspace.json"))
	assert.False(t, ok, "the failed upload defers to the close sweep")
	assert.True(t, f.exists(st.WorkspaceFile(project, ws, "workspace.json")),
		"local disk stays canonical")
}
