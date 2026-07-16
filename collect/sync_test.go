package collect_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/remote/remotetest"
	"go.jacobcolvin.com/hcp_archiver/seal"
	"go.jacobcolvin.com/hcp_archiver/store"
)

const (
	syncPrefix = "hcp"
	syncOrg    = "org"
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

// newSyncFixture builds a [syncFixture] over the standard test prefix.
func newSyncFixture(t *testing.T) syncFixture {
	t.Helper()

	root := t.TempDir()
	st := store.New(root)

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	cfg := remote.Config{Prefix: syncPrefix}
	fake := remotetest.New()

	client, err := remote.New(t.Context(), cfg,
		remote.WithBucket(fake.Bucket()), remote.WithRetry(0, 0))
	require.NoError(t, err)

	env := collect.NewEnv(nil, st, ledger,
		collect.WithRemote(client, cfg, syncOrg),
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

// sealBundle seals one member into a real verified bundle (zip plus sidecar)
// at relBundle, so the eviction path sees exactly what [seal.Seal] produces
// and its proof gate has a sidecar to verify against.
func (f syncFixture) sealBundle(t *testing.T, relBundle, memberName string, content []byte) {
	t.Helper()

	src := filepath.Join(t.TempDir(), "member")
	require.NoError(t, os.WriteFile(src, content, 0o600))

	_, err := seal.Seal(f.store.AbsPath(relBundle), []seal.Member{{
		Name:     memberName,
		Source:   src,
		Compress: true,
	}})
	require.NoError(t, err)
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
				f.sealBundle(t, bundles+"/logs.gen0001.zip",
					"projects/prod/workspaces/api/runs/run-1/plan.log", []byte("plan output"))
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

			f := newSyncFixture(t)

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

// TestSyncArchiveMirrorsEveryMeaningfulFile is the mirror's completeness
// contract: after a settled tree's sweep, the remote holds every file with
// meaning outside this tool — one representative per store path builder, the
// evicted cold surfaces, the remote marker, the ledger snapshots — and nothing
// tool-internal (staging temps, the flock target, the replay logs).
//
// The seeds pass through the store's real path builders, and a reflection
// sweep over [*store.Store] requires each builder to appear in a seed (or in
// the documented non-path exemptions), so a new archive surface cannot land
// without deciding its mirror fate here.
func TestSyncArchiveMirrorsEveryMeaningfulFile(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	st := f.store

	const (
		project = "prod"
		ws      = "api"
		stack   = "net"
	)

	svCreated := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)

	covered := make(map[string]struct{})

	var mirrored []string

	// Each seed writes one representative file at relPath and credits the
	// store builders that shaped it toward the completeness sweep below.
	seed := func(relPath string, builders ...string) {
		f.write(t, relPath, []byte("payload for "+relPath))

		mirrored = append(mirrored, relPath)

		for _, b := range builders {
			covered[b] = struct{}{}
		}
	}

	// Org-level surfaces.
	seed(st.Org(), "Org")
	seed(st.Memberships(), "Memberships")
	seed(st.User("user-1"), "User")
	seed(st.GitHubAppInstallations(), "GitHubAppInstallations")
	seed(st.RunTasks(), "RunTasks")
	seed(st.TokenTTLPolicies(), "TokenTTLPolicies")
	seed(st.ReservedTagKeys(), "ReservedTagKeys")
	seed(st.TeamFile("team-1", "team.json"), "TeamFile")
	seed(st.OAuthClientFile("oc-1", "oauth-client.json"), "OAuthClientFile")
	seed(st.OAuthTokenFile("oc-1", "ot-1"), "OAuthTokenFile")
	seed(st.VariableSetFile("varset-1", "variable-set.json"), "VariableSetFile")
	seed(st.PolicySetFile("polset-1", "policy-set.json"), "PolicySetFile")
	seed(st.Policy("pol-1", "json"), "Policy")
	seed(st.Policy("pol-1", "sentinel"))
	seed(st.AgentPool("apool-1"), "AgentPool")
	seed(st.AuditTrailFile("config.json"), "AuditTrailFile")
	seed(st.Join(st.AuditTrailDir(), "page-0001.json"), "AuditTrailDir", "Join")
	seed(st.HYOKConfigurationFile("hyokc-1", "hyok-configuration.json"), "HYOKConfigurationFile")
	seed(st.HYOKKeyVersionFile("hyokc-1", "keyv-1"), "HYOKKeyVersionFile")

	// The private registry.
	seed(st.RegistryModuleFile("ns", "vpc", "aws", "module.json"), "RegistryModuleFile")
	seed(st.RegistryNoCodeModule("nocode-1"), "RegistryNoCodeModule")
	seed(st.RegistryNoCodeModuleVariables("nocode-1"), "RegistryNoCodeModuleVariables")
	seed(st.RegistryProviderFile("ns", "custom", "provider.json"), "RegistryProviderFile")
	seed(st.RegistryGPGKey("ns", "key-1"), "RegistryGPGKey")

	// Projects, workspaces, and their run and state history.
	seed(st.ProjectFile(project, "project.json"), "ProjectFile", "ProjectDir")
	seed(st.WorkspaceFile(project, ws, "workspace.json"), "WorkspaceFile", "WorkspaceDir")
	seed(st.WorkspaceFile(project, ws, "readme.md"))
	seed(st.StateVersionFile(project, ws, svCreated, "sv-1", "tfstate.json"),
		"StateVersionFile", "StateVersionStem", "StateVersionDir")
	seed(st.RunFile(project, ws, "run-1", "run.json"), "RunFile", "RunDir")
	seed(st.RunFile(project, ws, "run-1", "plan.log"))
	seed(st.Join(st.RollupDir(project, ws), "runs.ndjson"), "RollupDir")

	// Stacks, down to the per-step artifacts.
	seed(st.StackFile(project, stack, "stack.json"), "StackFile", "StackDir")
	seed(st.StackConfigurationFile(project, stack, "sc-1", "configuration.json"),
		"StackConfigurationFile")
	seed(st.StackDeploymentGroupFile(project, stack, "sc-1", "sdg-1", "group.json"),
		"StackDeploymentGroupFile", "StackDeploymentGroupDir")
	seed(st.StackRunFile(project, stack, "sc-1", "sdg-1", "sr-1", "run.json"), "StackRunFile")
	seed(st.StackStepFile(project, stack, "sc-1", "sdg-1", "sr-1", "step-1", "plan.json"),
		"StackStepFile")
	seed(st.StackDeploymentFile(project, stack, "production", "deployment.json"),
		"StackDeploymentFile")
	seed(st.StackStateFile(project, stack, "production", "42"), "StackStateFile")

	// The cold surfaces reach the remote by eviction rather than sync: a
	// sealed bundle proven by its sidecar (the sidecar itself syncs), and a
	// configuration-version tarball proven by its done ledger entry.
	bundleZip := st.Join(st.BundleDir(project, ws), "logs.gen0001.zip")
	f.sealBundle(t, bundleZip, st.RunFile(project, ws, "run-9", "apply.log"), []byte("sealed apply output"))

	mirrored = append(mirrored, bundleZip, bundleZip+seal.SidecarSuffix)
	covered["BundleDir"] = struct{}{}

	tarball := st.ConfigVersionTarball("cv-1")
	f.writeDone(t, tarball, []byte("payload for "+tarball))

	mirrored = append(mirrored, tarball)
	covered["ConfigVersionTarball"] = struct{}{}

	// Files written outside the store's builders that still matter beyond this
	// tool: the marker a viewer needs to reach offloaded bundles, and the
	// ledger snapshots recording what proved each artifact.
	seed(remote.MarkerName)
	seed(manifest.LedgerDirName + "/snapshot.json")
	seed(st.Join(st.WorkspaceDir(project, ws), manifest.LedgerDirName, "snapshot.json"))

	// Tool-internal files carry no meaning outside this tool and stay off the
	// mirror: staging temps a crash left behind and the per-shard replay logs.
	// The org-root flock target is already on disk from the fixture's
	// manifest.Load, so the sweep sees all three exclusion shapes.
	f.write(t, "projects/.atomicfile-crash.tmp", []byte("partial write"))
	f.write(t, manifest.LedgerDirName+"/"+manifest.LogFileName, []byte("{}"))
	f.write(t, st.Join(st.WorkspaceDir(project, ws), manifest.LedgerDirName, manifest.LogFileName),
		[]byte("{}"))

	// The completeness sweep: every exported store method either shaped a seed
	// above or is exempt as a non-path helper.
	notPathBuilders := map[string]struct{}{
		"Root":           {}, // absolute filesystem accessor
		"AbsPath":        {}, // relative-to-absolute translation
		"Exists":         {}, // filesystem query
		"WriteJSON":      {}, // writer, not a path builder
		"WriteJSONBytes": {}, // writer, not a path builder
		"WriteBytes":     {}, // writer, not a path builder
		"WriteReader":    {}, // writer, not a path builder
	}

	for method := range reflect.TypeFor[*store.Store]().Methods() {
		name := method.Name

		if _, exempt := notPathBuilders[name]; exempt {
			continue
		}

		_, ok := covered[name]
		assert.True(t, ok,
			"store method %s shapes archive paths this inventory never seeds;"+
				" add a seed for its files or a documented exemption", name)
	}

	stats := f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)

	for _, relPath := range mirrored {
		_, ok := f.fake.Object(f.key(relPath))
		assert.True(t, ok, "meaningful file %s must be mirrored to the remote", relPath)
	}

	// Presence of every mirrored path plus this length check pins the remote
	// key set exactly: nothing internal leaked.
	assert.Len(t, f.fake.Keys(), len(mirrored),
		"the remote must hold exactly the meaningful files")

	assert.Equal(t, 2, stats.Evicted, "the bundle zip and the proven tarball evict")
	assert.Equal(t, len(mirrored)-2, stats.Uploaded, "everything else syncs in place")
	assert.False(t, f.exists(t, bundleZip), "an evicted bundle leaves disk")
	assert.False(t, f.exists(t, tarball), "an evicted tarball leaves disk")
}

func TestSyncArchiveStreamsLargeFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	st := store.New(root)

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	cfg := remote.Config{Prefix: syncPrefix}
	fake := remotetest.New()

	client, err := remote.New(t.Context(), cfg,
		remote.WithBucket(fake.Bucket()), remote.WithRetry(0, 0))
	require.NoError(t, err)

	// A threshold of one byte routes every synced file through the streamed
	// path, standing in for a gigabyte-scale roll-up without the fixture.
	env := collect.NewEnv(nil, st, ledger,
		collect.WithRemote(client, cfg, syncOrg),
		collect.WithLogger(slog.New(slog.DiscardHandler)),
		collect.WithStreamThreshold(1),
	)

	const rollup = "projects/prod/workspaces/api/rollups/runs.ndjson"

	data := []byte(`{"path":"runs/run-1/run.json"}` + "\n")
	_, err = st.WriteBytes(rollup, data)
	require.NoError(t, err)

	stats := env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Uploaded)

	obj, ok := fake.Object(syncPrefix + "/" + syncOrg + "/" + rollup)
	require.True(t, ok)
	assert.Equal(t, data, obj.Data, "the streamed upload reassembles byte for byte")
	assert.Equal(t, remotetest.MD5Sum(data), obj.MD5,
		"the streamed write carries the digest the incremental gate compares")
	assert.Equal(t, manifest.SignatureOf(data).Hash, obj.Metadata["sha256"],
		"the streamed write records its sha256 as metadata")

	// The second sweep must see the streamed copy as settled: the recorded
	// digest, not just size, drives the skip.
	stats = env.SyncArchive(t.Context())
	assert.Zero(t, stats.Uploaded)
	assert.Equal(t, 1, stats.Skipped)
}

func TestSyncArchiveEvictionRecordsDigests(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	data := []byte("tarball bytes")
	f.writeDone(t, "config-versions/cv-1.tar.gz", data)

	stats := f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Evicted)

	obj, ok := f.fake.Object(f.key("config-versions/cv-1.tar.gz"))
	require.True(t, ok)
	assert.Equal(t, manifest.SignatureOf(data).Hash, obj.Metadata["sha256"],
		"the evicted object records the proof's sha256 as metadata")
	assert.Equal(t, remotetest.MD5Sum(data), obj.MD5,
		"the evicted object records a comparable md5")
}

func TestSyncArchiveRefusesRottedTarball(t *testing.T) {
	t.Parallel()

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t)

	// The ledger proved one payload; the file on disk now carries another of
	// the same size (bit rot, a partial restore). The custody transfer must
	// refuse it: rotted bytes must never become the archive's only copy.
	f.writeDone(t, tarball, []byte("proven tarball bytes"))
	f.write(t, tarball, []byte("rotted tarball bytes"))

	stats := f.env.SyncArchive(t.Context())

	assert.Equal(t, 1, stats.Failed, "a failed proof counts and re-reports every run")
	assert.Zero(t, stats.Evicted)
	assert.True(t, f.exists(t, tarball), "the suspect file stays local for inspection")

	_, ok := f.fake.Object(f.key(tarball))
	assert.False(t, ok, "rotted bytes are never uploaded")
}

func TestSyncArchiveRefusesCorruptedBundle(t *testing.T) {
	t.Parallel()

	const zip = "projects/prod/workspaces/api/bundles/logs.gen0001.zip"

	f := newSyncFixture(t)

	// A real seal, then rot: the zip's bytes change under its sidecar. The
	// member-by-member re-verify at eviction must catch it.
	f.sealBundle(t, zip, "projects/prod/workspaces/api/runs/run-1/plan.log", []byte("plan output"))
	f.write(t, zip, []byte("rotted zip bytes"))

	stats := f.env.SyncArchive(t.Context())

	assert.Equal(t, 1, stats.Failed)
	assert.True(t, f.exists(t, zip), "the suspect bundle stays local for inspection")

	_, ok := f.fake.Object(f.key(zip))
	assert.False(t, ok, "a bundle that fails its sidecar proof is never uploaded")
}

func TestSyncArchiveEvictRefusesForeignRemoteCopy(t *testing.T) {
	t.Parallel()

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t)

	local := []byte("proven tarball bytes")
	foreign := []byte("foreign remote bytes")

	f.writeDone(t, tarball, local)

	// A remote object already answers the eviction key with the same size but
	// different recorded content: deleting the local file would leave the
	// wrong bytes as the archive's only copy.
	f.fake.SetObject(f.key(tarball), remotetest.Object{
		Data:     foreign,
		Metadata: map[string]string{"sha256": manifest.SignatureOf(foreign).Hash},
	})

	stats := f.env.SyncArchive(t.Context())

	assert.Equal(t, 1, stats.Failed)
	assert.True(t, f.exists(t, tarball), "a digest mismatch keeps the local file canonical")

	obj, _ := f.fake.Object(f.key(tarball))
	assert.Equal(t, foreign, obj.Data, "the sweep never overwrites remote history at the key")
}

func TestSyncArchiveEvictDigestMatchSkipsReupload(t *testing.T) {
	t.Parallel()

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t)

	data := []byte("proven tarball bytes")
	f.writeDone(t, tarball, data)

	// Crash point: a prior run uploaded (recording digests) and died before
	// the local delete. The resumed sweep matches digest for digest and
	// evicts without re-uploading.
	f.fake.SetObject(f.key(tarball), remotetest.Object{
		Data:     data,
		MD5:      remotetest.MD5Sum(data),
		Metadata: map[string]string{"sha256": manifest.SignatureOf(data).Hash},
	})

	stats := f.env.SyncArchive(t.Context())

	require.Zero(t, stats.Failed)
	assert.Equal(t, 1, stats.Evicted)
	assert.Zero(t, f.fake.PutCalls(), "a digest-confirmed remote copy is not re-uploaded")
	assert.False(t, f.exists(t, tarball), "the local copy is released")
}

func TestSyncArchiveIncrementalGate(t *testing.T) {
	t.Parallel()

	const relPath = "org.json"

	content := []byte(`{"org":"acme"}`)

	tests := map[string]struct {
		remoteObj  *remotetest.Object
		wantUpload bool
		wantHeads  int
	}{
		"an absent key uploads": {
			wantUpload: true,
		},
		"a size difference uploads": {
			remoteObj:  &remotetest.Object{Data: []byte("longer stale copy")},
			wantUpload: true,
		},
		"an equal size with a matching digest skips without a Head": {
			remoteObj: &remotetest.Object{Data: content, MD5: remotetest.MD5Sum(content)},
		},
		"an equal size with a differing digest uploads": {
			remoteObj: &remotetest.Object{
				Data: []byte(`{"org":"evil"}`),
				MD5:  remotetest.MD5Sum([]byte(`{"org":"evil"}`)),
			},
			wantUpload: true,
		},
		"an equal size with no recorded digest is trusted after one Head": {
			// A store that records no digest at all leaves size the whole
			// gate, the documented degradation; the Head is the fallback for
			// backends whose listings omit a digest the object still carries.
			remoteObj: &remotetest.Object{Data: []byte(`{"org":"evil"}`)},
			wantHeads: 1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newSyncFixture(t)
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
				"a digest-carrying inventory entry must settle without a Head")
		})
	}
}

func TestSyncArchiveSecondSweepUploadsNothing(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
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
		"our own Put records a digest, so the gate settles from the inventory alone")
}

func TestSyncArchiveTarballCrashAfterUploadEvictsWithoutReupload(t *testing.T) {
	t.Parallel()

	const relPath = "config-versions/cv-1.tar.gz"

	content := []byte("tarball bytes")

	f := newSyncFixture(t)
	f.writeDone(t, relPath, content)

	// The crash point: a prior sweep uploaded the tarball but died before the
	// local delete. The resumed sweep must find the remote copy, verify size,
	// and delete local without uploading again.
	f.fake.SetObject(f.key(relPath), remotetest.Object{Data: content})

	stats := f.env.SyncArchive(t.Context())

	assert.Equal(t, 1, stats.Evicted)
	assert.Zero(t, f.fake.PutCalls(), "a confirmed remote copy is not re-uploaded")
	assert.False(t, f.exists(t, relPath), "the local tarball is still evicted")
}

func TestSyncArchivePrunesStaleRemoteKeys(t *testing.T) {
	t.Parallel()

	const stale = "projects/prod/workspaces/api/runs/run-1/run.json"

	f := newSyncFixture(t)

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

	f := newSyncFixture(t)

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

	f := newSyncFixture(t)

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

	f := newSyncFixture(t)

	// A remote copy exists but the local walk sees no file at all (a wrong or
	// wiped root): the guard must keep the prune from emptying the mirror.
	f.fake.SetObject(f.key("org.json"), remotetest.Object{Data: []byte("x")})

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Pruned)
	assert.Empty(t, f.fake.Deleted())
}

func TestSyncArchiveCanceledContextUploadsNothing(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
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

	f := newSyncFixture(t)

	// A sealed zip with its sidecar is eligible for eviction, so the evict loop
	// runs OffloadFile, whose first Head is where the cancellation surfaces.
	f.sealBundle(t, zip, "projects/prod/workspaces/api/runs/run-1/plan.log", []byte("plan output"))

	ctx, cancel := context.WithCancel(t.Context())

	// Cancel mid-offload: the Head probe inside OffloadFile trips the wind-down,
	// so the eviction error must not be counted a failure.
	f.fake.HeadHook = func(context.Context) { cancel() }

	stats := f.env.SyncArchive(ctx)

	assert.Zero(t, stats.Failed, "an in-flight cancellation is the wind-down, not a failure")
	assert.Zero(t, stats.Evicted, "the offload did not complete")
	assert.True(t, f.exists(t, zip), "the local bundle stays canonical for the next run")
}

func TestSyncArchivePruneCancellationIsNotFailure(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	// A stale remote key with nothing local behind it makes the prune step
	// issue a delete; the cancellation surfaces there, after the uploads.
	f.fake.SetObject(f.key("projects/old/workspaces/gone/workspace.json"),
		remotetest.Object{Data: []byte("stale")})
	f.write(t, "org.json", []byte(`{"org":"acme"}`))

	ctx, cancel := context.WithCancel(t.Context())
	f.fake.DeleteHook = func(context.Context) { cancel() }

	stats := f.env.SyncArchive(ctx)

	assert.Zero(t, stats.Failed, "a cancellation mid-prune is the wind-down, not a failure")
	assert.Zero(t, stats.Pruned)
}

func TestSyncArchivePerFileFailureWarnsAndContinues(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.write(t, "org.json", []byte(`{"org":"acme"}`))
	f.write(t, "memberships.json", []byte(`[]`))

	f.fake.PutErr = assert.AnError

	stats := f.env.SyncArchive(t.Context())

	assert.Equal(t, 2, stats.Failed, "each failed file counts and the sweep continues")
	assert.Zero(t, stats.Uploaded)
}
