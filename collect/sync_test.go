package collect_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/remote/remotetest"
	"go.jacobcolvin.com/hcp_archiver/store"
)

const (
	syncBucket     = "sync-bucket"
	syncPrefix     = "hcp"
	syncOrg        = "org"
	syncEvictClass = "DEEP_ARCHIVE"
	syncSyncClass  = "STANDARD_IA"
)

// syncFixture is a remote-configured environment over a real store and ledger,
// backed by an in-memory fake, so the close sweep can be exercised without a
// network.
type syncFixture struct {
	env    *collect.Env
	store  *store.Store
	ledger *manifest.Ledger
	fake   *remotetest.Fake
}

// newSyncFixture builds a [syncFixture]; extra remote settings apply on top of
// the bucket and prefix.
func newSyncFixture(t *testing.T, cfg remote.Config) syncFixture {
	t.Helper()

	root := t.TempDir()
	st := store.New(root)

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	cfg.Bucket = syncBucket
	cfg.Prefix = syncPrefix
	fake := remotetest.New(syncBucket)

	client, err := remote.New(t.Context(), cfg, remote.WithS3API(fake))
	require.NoError(t, err)

	env := collect.NewEnv(nil, st, ledger,
		collect.WithRemote(client, cfg, syncOrg),
		collect.WithStorageClasses(syncEvictClass, syncSyncClass),
		collect.WithLogger(slog.New(slog.DiscardHandler)),
	)

	return syncFixture{env: env, store: st, ledger: ledger, fake: fake}
}

// key composes the remote key mirroring an archive-relative path.
func (f syncFixture) key(relPath string) string {
	return syncPrefix + "/" + syncOrg + "/" + relPath
}

// write commits a loose file without a ledger record.
func (f syncFixture) write(t *testing.T, relPath string, data []byte) {
	t.Helper()

	_, err := f.store.WriteBytes(relPath, data)
	require.NoError(t, err)
}

// writeDone commits a loose file and records it done, the state of a settled
// artifact.
func (f syncFixture) writeDone(t *testing.T, relPath string, data []byte) {
	t.Helper()

	f.write(t, relPath, data)
	f.ledger.RecordDone(relPath, manifest.SignatureOf(data))
}

func (f syncFixture) exists(t *testing.T, relPath string) bool {
	t.Helper()

	ok, err := f.store.Exists(relPath)
	require.NoError(t, err)

	return ok
}

func TestSyncArchiveNoRemoteIsNoop(t *testing.T) {
	t.Parallel()

	env, _, _ := newEnv(t)

	assert.Zero(t, env.SyncArchive(t.Context()))
}

func TestSyncArchiveClassification(t *testing.T) {
	t.Parallel()

	const bundles = "projects/prod/workspaces/api/bundles"

	tests := map[string]struct {
		seed        func(t *testing.T, f syncFixture)
		relPath     string
		wantRemote  bool
		wantLocal   bool
		wantEvicted bool
	}{
		"a staging temp is never uploaded": {
			relPath:   "projects/.atomicfile-123.tmp",
			wantLocal: true,
		},
		"the ledger flock target is never uploaded": {
			relPath:   ".ledger/lock",
			wantLocal: true,
		},
		"a shard replay log is never uploaded": {
			relPath:   ".ledger/log.ndjson",
			wantLocal: true,
		},
		"a shard snapshot syncs": {
			relPath:    ".ledger/snapshot.json",
			wantRemote: true,
			wantLocal:  true,
		},
		"a sealed zip with its sidecar evicts": {
			seed: func(t *testing.T, f syncFixture) {
				t.Helper()
				f.write(t, bundles+"/logs.gen0001.zip.sidecar.ndjson", []byte(`{"name":"x"}`))
			},
			relPath:     bundles + "/logs.gen0001.zip",
			wantRemote:  true,
			wantEvicted: true,
		},
		"an orphan zip without a sidecar stays local, never uploaded": {
			relPath:   bundles + "/logs.gen0002.zip",
			wantLocal: true,
		},
		"a settled tarball evicts": {
			seed: func(t *testing.T, f syncFixture) {
				t.Helper()
				f.ledger.RecordDone("config-versions/cv-1.tar.gz",
					manifest.SignatureOf([]byte("tarball")))
			},
			relPath:     "config-versions/cv-1.tar.gz",
			wantRemote:  true,
			wantEvicted: true,
		},
		"a tarball with no ledger entry stays local, never uploaded": {
			relPath:   "config-versions/cv-2.tar.gz",
			wantLocal: true,
		},
		"a tarball whose size mismatches its signature stays local": {
			seed: func(t *testing.T, f syncFixture) {
				t.Helper()
				f.ledger.RecordDone("config-versions/cv-3.tar.gz",
					manifest.Signature{Hash: "h", Size: 999})
			},
			relPath:   "config-versions/cv-3.tar.gz",
			wantLocal: true,
		},
		"an org metadata file syncs": {
			relPath:    "org.json",
			wantRemote: true,
			wantLocal:  true,
		},
		"a loose run.json syncs": {
			relPath:    "projects/prod/workspaces/api/runs/run-1/run.json",
			wantRemote: true,
			wantLocal:  true,
		},
		"a roll-up syncs": {
			relPath:    "projects/prod/workspaces/api/rollups/runs.ndjson",
			wantRemote: true,
			wantLocal:  true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newSyncFixture(t, remote.Config{})

			content := []byte("tarball")
			f.write(t, tc.relPath, content)

			if tc.seed != nil {
				tc.seed(t, f)
			}

			stats := f.env.SyncArchive(t.Context())

			_, remotePresent := f.fake.Object(f.key(tc.relPath))
			assert.Equal(t, tc.wantRemote, remotePresent, "remote presence")
			assert.Equal(t, tc.wantLocal, f.exists(t, tc.relPath), "local presence")

			if tc.wantEvicted {
				assert.Equal(t, 1, stats.Evicted)
			} else {
				assert.Zero(t, stats.Evicted)
			}

			assert.Zero(t, stats.Failed)
		})
	}
}

func TestSyncArchiveIncrementalGate(t *testing.T) {
	t.Parallel()

	const relPath = "org.json"

	content := []byte(`{"org":"acme"}`)

	tests := map[string]struct {
		remoteObj        *remotetest.Object
		disableChecksums bool
		wantUpload       bool
		wantHeads        int
	}{
		"an absent key uploads": {
			wantUpload: true,
		},
		"a size difference uploads": {
			remoteObj:  &remotetest.Object{Data: []byte("longer stale copy"), ETag: "whatever"},
			wantUpload: true,
		},
		"an equal size with a matching MD5 ETag skips without a Head": {
			remoteObj: &remotetest.Object{Data: content, ETag: remotetest.MD5Hex(content)},
		},
		"a matching uppercase-hex ETag still skips": {
			remoteObj: &remotetest.Object{Data: content, ETag: strings.ToUpper(remotetest.MD5Hex(content))},
		},
		"an equal size with a differing MD5 ETag uploads": {
			remoteObj: &remotetest.Object{
				Data: []byte(`{"org":"evil"}`),
				ETag: remotetest.MD5Hex([]byte(`{"org":"evil"}`)),
			},
			wantUpload: true,
		},
		"an uncomparable ETag falls back to the Head checksum": {
			// A composite ETag with a stale same-length body: only the store's
			// recorded SHA-256 can tell them apart, so it must be a real 32-byte
			// digest of the stale remote body, not a placeholder.
			remoteObj: &remotetest.Object{
				Data:           []byte(`{"org":"evil"}`),
				ETag:           "0123456789abcdef0123456789abcdef-2",
				ChecksumSHA256: remotetest.SHA256Base64([]byte(`{"org":"evil"}`)),
			},
			wantUpload: true,
			wantHeads:  1,
		},
		"checksums off degrades to size-only": {
			remoteObj:        &remotetest.Object{Data: []byte(`{"org":"evil"}`), ETag: "opaque-not-md5"},
			disableChecksums: true,
			wantHeads:        1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newSyncFixture(t, remote.Config{DisableChecksums: tc.disableChecksums})
			f.write(t, relPath, content)

			if tc.remoteObj != nil {
				f.fake.SetObject(f.key(relPath), *tc.remoteObj)
			}

			stats := f.env.SyncArchive(t.Context())
			require.Zero(t, stats.Failed)

			obj, ok := f.fake.Object(f.key(relPath))
			require.True(t, ok)

			if tc.wantUpload {
				assert.Positive(t, stats.Uploaded)
				assert.Equal(t, content, obj.Data, "the local bytes replace the stale copy")
			} else {
				assert.Zero(t, stats.Uploaded)
				assert.Positive(t, stats.Skipped)
			}

			assert.Equal(t, tc.wantHeads, f.fake.HeadCalls(),
				"a comparable inventory ETag must settle without a Head")
		})
	}
}

func TestSyncArchiveAppliesStorageClasses(t *testing.T) {
	t.Parallel()

	const (
		zip      = "projects/prod/workspaces/api/bundles/logs.gen0001.zip"
		loose    = "org.json"
		zipBytes = "zip bytes"
	)

	f := newSyncFixture(t, remote.Config{})

	f.write(t, zip, []byte(zipBytes))
	f.write(t, zip+".sidecar.ndjson", []byte(`{"name":"x"}`))
	f.write(t, loose, []byte(`{"org":"acme"}`))

	stats := f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)

	evicted, ok := f.fake.Object(f.key(zip))
	require.True(t, ok)
	assert.Equal(t, syncEvictClass, evicted.StorageClass,
		"an evicted surface takes the eviction class")

	synced, ok := f.fake.Object(f.key(loose))
	require.True(t, ok)
	assert.Equal(t, syncSyncClass, synced.StorageClass,
		"a synced file takes the sync class")
}

func TestSyncArchiveSecondSweepUploadsNothing(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t, remote.Config{})
	f.write(t, "org.json", []byte(`{"org":"acme"}`))
	f.write(t, "projects/prod/workspaces/api/workspace.json", []byte(`{"ws":"api"}`))

	first := f.env.SyncArchive(t.Context())
	require.Equal(t, 2, first.Uploaded)
	require.Zero(t, first.Failed)

	puts := f.fake.PutCalls()

	second := f.env.SyncArchive(t.Context())
	assert.Zero(t, second.Uploaded, "an unchanged tree re-uploads nothing")
	assert.Equal(t, 2, second.Skipped)
	assert.Equal(t, puts, f.fake.PutCalls())
	assert.Zero(t, f.fake.HeadCalls(),
		"our own Put ETags are plain MD5s, so the gate settles from the inventory alone")
}

func TestSyncArchiveTarballCrashAfterUploadEvictsWithoutReupload(t *testing.T) {
	t.Parallel()

	const relPath = "config-versions/cv-1.tar.gz"

	content := []byte("tarball bytes")

	f := newSyncFixture(t, remote.Config{})
	f.writeDone(t, relPath, content)

	// The crash point: a prior sweep uploaded the tarball but died before the
	// local delete. The resumed sweep must find the remote copy, verify size,
	// and delete local without uploading again.
	f.fake.SetObject(f.key(relPath), remotetest.Object{Data: content, ETag: remotetest.MD5Hex(content)})

	stats := f.env.SyncArchive(t.Context())

	assert.Equal(t, 1, stats.Evicted)
	assert.Zero(t, f.fake.PutCalls()+f.fake.Completed(), "a confirmed remote copy is not re-uploaded")
	assert.False(t, f.exists(t, relPath), "the local tarball is still evicted")
}

func TestSyncArchivePrunesStaleRemoteKeys(t *testing.T) {
	t.Parallel()

	const stale = "projects/prod/workspaces/api/runs/run-1/run.json"

	f := newSyncFixture(t, remote.Config{})

	// The mirror holds a loose run.json from before the seal coalesced it; the
	// local walk no longer sees it, so it must be pruned or a later restore
	// would shadow the newer roll-up line.
	f.fake.SetObject(f.key(stale), remotetest.Object{Data: []byte(`{"id":"run-1"}`)})

	f.write(t, "projects/prod/workspaces/api/rollups/runs.ndjson", []byte(`{"path":"x"}`))

	stats := f.env.SyncArchive(t.Context())

	assert.Equal(t, 1, stats.Pruned)
	assert.Equal(t, []string{f.key(stale)}, f.fake.Deleted())

	_, present := f.fake.Object(f.key(stale))
	assert.False(t, present)
}

func TestSyncArchivePruneExemptsEvictedSurfaces(t *testing.T) {
	t.Parallel()

	const (
		zip     = "projects/prod/workspaces/api/bundles/logs.gen0001.zip"
		tarball = "config-versions/cv-1.tar.gz"
	)

	f := newSyncFixture(t, remote.Config{})

	// Both cold surfaces were evicted by an earlier run: remote-only, with the
	// sidecar (bundle) and the done ledger entry (tarball) as their local
	// proof. Neither may be pruned.
	f.fake.SetObject(f.key(zip), remotetest.Object{Data: []byte("zip bytes")})
	f.fake.SetObject(f.key(tarball), remotetest.Object{Data: []byte("tar bytes")})

	f.write(t, zip+".sidecar.ndjson", []byte(`{"name":"x"}`))
	f.ledger.RecordDone(tarball, manifest.SignatureOf([]byte("tar bytes")))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Pruned)
	assert.Empty(t, f.fake.Deleted())
}

func TestSyncArchivePruneSurvivesLocalMetadataLoss(t *testing.T) {
	t.Parallel()

	const (
		zip     = "projects/prod/workspaces/api/bundles/logs.gen0001.zip"
		tarball = "config-versions/cv-1.tar.gz"
	)

	f := newSyncFixture(t, remote.Config{})

	// The evicted surfaces' local proof is gone — the sidecar lost with a
	// deleted subtree, the ledger wiped to reset state. The remote copies are
	// the archive's only bytes, so the eviction shape alone must exempt them.
	f.fake.SetObject(f.key(zip), remotetest.Object{Data: []byte("zip bytes")})
	f.fake.SetObject(f.key(tarball), remotetest.Object{Data: []byte("tar bytes")})

	// An unrelated local file keeps the walk non-empty, so the prune step
	// itself still runs.
	f.write(t, "org.json", []byte(`{"org":"acme"}`))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Pruned)
	assert.Empty(t, f.fake.Deleted())
}

func TestSyncArchiveEmptyWalkPrunesNothing(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t, remote.Config{})

	// A remote copy exists but the local walk sees no file at all (a wrong or
	// wiped root): the guard must keep the prune from emptying the mirror.
	f.fake.SetObject(f.key("org.json"), remotetest.Object{Data: []byte("x")})

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Pruned)
	assert.Empty(t, f.fake.Deleted())
}

func TestSyncArchiveCanceledContextUploadsNothing(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t, remote.Config{})
	f.write(t, "org.json", []byte(`{"org":"acme"}`))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	stats := f.env.SyncArchive(ctx)

	assert.Zero(t, stats.Uploaded)
	assert.Empty(t, f.fake.Keys())
}

func TestSyncArchiveEvictCancellationIsNotFailure(t *testing.T) {
	t.Parallel()

	const zip = "projects/prod/workspaces/api/bundles/logs.gen0001.zip"

	f := newSyncFixture(t, remote.Config{})

	// A sealed zip with its sidecar is eligible for eviction, so the evict loop
	// runs OffloadFile, whose first Head is where the cancellation surfaces.
	f.write(t, zip, []byte("zip bytes"))
	f.write(t, zip+".sidecar.ndjson", []byte(`{"name":"x"}`))

	ctx, cancel := context.WithCancel(t.Context())

	// Cancel mid-offload: the Head probe inside OffloadFile trips the wind-down,
	// so the eviction error must not be counted a failure.
	f.fake.HeadHook = func(context.Context) { cancel() }

	stats := f.env.SyncArchive(ctx)

	assert.Zero(t, stats.Failed, "an in-flight cancellation is the wind-down, not a failure")
	assert.Zero(t, stats.Evicted, "the offload did not complete")
	assert.True(t, f.exists(t, zip), "the local bundle stays canonical for the next run")
}

func TestSyncArchivePerFileFailureWarnsAndContinues(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t, remote.Config{})
	f.write(t, "org.json", []byte(`{"org":"acme"}`))
	f.write(t, "memberships.json", []byte(`[]`))

	f.fake.PutErr = assert.AnError

	stats := f.env.SyncArchive(t.Context())

	assert.Equal(t, 2, stats.Failed, "each failed file counts and the sweep continues")
	assert.Zero(t, stats.Uploaded)
}
