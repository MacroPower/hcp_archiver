package collect_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/collect/collecttest"
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

// newSyncFixture builds a [syncFixture] over the standard test prefix. The
// client retries nothing unless opts say otherwise, so an injected fault
// surfaces as the failure it models rather than being absorbed.
func newSyncFixture(t *testing.T, opts ...remote.Option) syncFixture {
	t.Helper()

	root := t.TempDir()
	st := store.New(root)

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	cfg := remote.Config{Prefix: syncPrefix}
	fake := remotetest.New()

	client, err := remote.New(t.Context(), cfg,
		append([]remote.Option{remote.WithBucket(fake.Bucket()), remote.WithRetry(0, 0)}, opts...)...)
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

// writeDone commits a loose file, records it done, and flushes, the state of
// a settled artifact whose record a healthy run has already made durable.
// Eviction gates on that durability, so the fixture models it.
func (f syncFixture) writeDone(t *testing.T, relPath string, data []byte) {
	t.Helper()

	f.write(t, relPath, data)
	f.ledger.RecordDone(relPath, manifest.SignatureOf(data))
	require.NoError(t, f.ledger.Flush())
}

// resume marks the fixture's run as resumed: at least one ledger entry
// existed when the run opened, the state of every re-run against a real
// archive. The prune step acts only in that state (a run that opened an empty
// ledger against a non-empty mirror is a fresh or wrong root and refuses to
// prune), so prune tests model it explicitly.
func (f syncFixture) resume() {
	f.ledger.RecordSkipped(".fixture-prior-entry")
	f.ledger.StartRun()
}

func (f syncFixture) exists(t *testing.T, relPath string) bool {
	t.Helper()

	ok, err := f.store.Exists(relPath)
	require.NoError(t, err)

	return ok
}

// readStub reads and parses the eviction stub standing in for relPath.
func (f syncFixture) readStub(t *testing.T, relPath string) store.RemoteStub {
	t.Helper()

	data, err := os.ReadFile(f.store.AbsPath(store.RemoteStubPath(relPath)))
	require.NoError(t, err)

	var stub store.RemoteStub

	require.NoError(t, json.Unmarshal(data, &stub))

	return stub
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
		wantFailed  int
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
				require.NoError(t, f.ledger.Flush())
			},
			relPath:     "config-versions/cv-1.tar.gz",
			wantRemote:  true,
			wantEvicted: true,
		},
		"a tarball with no ledger entry stays local, never uploaded": {
			relPath:   "config-versions/cv-2.tar.gz",
			wantLocal: true,
		},
		"a tarball whose size mismatches its signature stays local and fails the sweep": {
			seed: func(t *testing.T, f syncFixture) {
				t.Helper()
				f.ledger.RecordDone("config-versions/cv-3.tar.gz",
					manifest.Signature{Hash: "h", Size: 999})
				require.NoError(t, f.ledger.Flush())
			},
			relPath:    "config-versions/cv-3.tar.gz",
			wantLocal:  true,
			wantFailed: 1,
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

			assert.Equal(t, tc.wantFailed, stats.Failed,
				"a suspect only-copy counts; every other outcome stays clean")
		})
	}
}

func TestSyncArchiveDrivesProgressHook(t *testing.T) {
	t.Parallel()

	const bundles = "projects/prod/workspaces/api/bundles"

	tests := map[string]struct {
		seed       func(t *testing.T, f syncFixture)
		wantFailed int
	}{
		"every settled file advances, evictions and syncs alike": {
			seed: func(t *testing.T, f syncFixture) {
				t.Helper()
				f.write(t, "org.json", []byte(`{"org":"org"}`))
				f.write(t, "projects/prod/workspaces/api/runs/run-1/run.json", []byte(`{}`))
				f.sealBundle(t, bundles+"/logs.gen0001.zip",
					"projects/prod/workspaces/api/runs/run-1/plan.log", []byte("plan output"))
				f.writeDone(t, "config-versions/cv-1.tar.gz", []byte("tarball"))
			},
		},
		"a suspect only-copy counts a failure but never settles": {
			seed: func(t *testing.T, f syncFixture) {
				t.Helper()
				f.write(t, "org.json", []byte(`{"org":"org"}`))
				f.write(t, "config-versions/cv-3.tar.gz", []byte("tarball"))
				f.ledger.RecordDone("config-versions/cv-3.tar.gz",
					manifest.Signature{Hash: "h", Size: 999})
				require.NoError(t, f.ledger.Flush())
			},
			wantFailed: 1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newSyncFixture(t)
			tc.seed(t, f)

			prog := &collecttest.RecordingSyncProgress{}
			stats := f.env.SyncArchive(t.Context(), collect.WithSyncProgress(prog))

			totals := prog.Totals()
			require.Len(t, totals, 1,
				"the total is seeded exactly once, after the tree is classified")
			assert.Positive(t, totals[0])
			assert.Equal(t, stats.Uploaded+stats.Skipped+stats.Evicted, totals[0],
				"with no per-file failures the settled outcomes sum to the total; suspects stay out")
			assert.Equal(t, totals[0], prog.Advanced(),
				"a clean sweep's advances sum to the seeded total")
			assert.Equal(t, tc.wantFailed, stats.Failed)
		})
	}
}

func TestSyncArchiveNoRemoteLeavesHookUntouched(t *testing.T) {
	t.Parallel()

	env, _, _ := newEnv(t)
	prog := &collecttest.RecordingSyncProgress{}

	assert.Zero(t, env.SyncArchive(t.Context(), collect.WithSyncProgress(prog)))
	assert.Empty(t, prog.Totals(), "without a remote the sweep returns before any classification")
	assert.Zero(t, prog.Advanced())
}

// TestSyncArchiveMirrorsEveryMeaningfulFile is the mirror's completeness
// contract: after a settled tree's sweep, the remote holds every file with
// meaning outside this tool (one representative per store path builder, the
// evicted cold surfaces, the remote marker, the ledger snapshots) and nothing
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
	seed(st.RegistryModuleFile("private", "ns", "vpc", "aws", "module.json"), "RegistryModuleFile")
	seed(st.RegistryNoCodeModule("nocode-1"), "RegistryNoCodeModule")
	seed(st.RegistryNoCodeModuleVariables("nocode-1"), "RegistryNoCodeModuleVariables")
	seed(st.RegistryProviderFile("private", "ns", "custom", "provider.json"), "RegistryProviderFile")
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
	seed(st.HistoryPath(st.WorkspaceFile(project, ws, "variables.json")), "HistoryPath")

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
		"BuryHistory":    {}, // writer, not a path builder
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

	// The run-wide tally credits the eviction's bytes; the per-pass stats
	// exclude them.
	assert.Zero(t, stats.Uploaded)
	assert.Zero(t, stats.UploadedBytes, "per-pass uploaded bytes exclude evictions")

	tally := f.env.RemoteTally()
	assert.Equal(t, 1, tally.Evicted)
	assert.Zero(t, tally.Uploaded)
	assert.Equal(t, int64(len(data)), tally.UploadedBytes,
		"the run tally includes the eviction's transferred bytes")
}

func TestSyncArchiveEvictedTarballLeavesAStub(t *testing.T) {
	t.Parallel()

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t)

	data := []byte("tarball bytes")
	f.writeDone(t, tarball, data)

	stats := f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Evicted)
	require.False(t, f.exists(t, tarball), "the evicted tarball is gone locally")

	// The stub is what the readers see in the tarball's place: without it the
	// object is indistinguishable from one never archived.
	stub := f.readStub(t, tarball)
	assert.Equal(t, store.RemoteStubVersion, stub.Version)
	assert.EqualValues(t, len(data), stub.Size, "the stub carries the evicted size")
	assert.Equal(t, manifest.SignatureOf(data).Hash, stub.SHA256,
		"the stub carries the digest the eviction proved")

	// The stub points at the mirror from outside it; a copy inside would be
	// stale the moment a restore brings the tarball back.
	_, ok := f.fake.Object(f.key(store.RemoteStubPath(tarball)))
	assert.False(t, ok, "an eviction stub is never uploaded")

	// A later sweep neither re-uploads it nor trips over it.
	stats = f.env.SyncArchive(t.Context())
	assert.Zero(t, stats.Failed)
	assert.Zero(t, stats.Uploaded)
}

func TestSyncArchiveEvictedBundleLeavesNoStub(t *testing.T) {
	t.Parallel()

	const zip = "projects/prod/workspaces/api/bundles/logs.gen0001.zip"

	f := newSyncFixture(t)

	f.sealBundle(t, zip, "projects/prod/workspaces/api/runs/run-1/plan.log", []byte("plan output"))

	stats := f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Evicted)

	// The sidecar already outlives the zip and says the same thing a stub
	// would, so a bundle gets none.
	assert.False(t, f.exists(t, store.RemoteStubPath(zip)))
	assert.True(t, f.exists(t, zip+seal.SidecarSuffix), "the sidecar is the bundle's trace")
}

func TestSyncArchiveStubFailureKeepsTheTarball(t *testing.T) {
	t.Parallel()

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t)

	f.writeDone(t, tarball, []byte("tarball bytes"))

	// A directory where the stub belongs makes the write fail the way a
	// permission problem or a full disk would. The eviction must abort before
	// the delete: releasing the only local copy while nothing local records
	// where it went is exactly what the stub exists to prevent.
	require.NoError(t, os.MkdirAll(f.store.AbsPath(store.RemoteStubPath(tarball)), 0o755))

	stats := f.env.SyncArchive(t.Context())

	assert.Equal(t, 1, stats.Failed, "an eviction that cannot leave a trace counts against the run")
	assert.Zero(t, stats.Evicted)
	assert.True(t, f.exists(t, tarball), "the tarball stays local rather than becoming untraceable")
}

func TestSyncArchiveEvictionRewritesAStaleStub(t *testing.T) {
	t.Parallel()

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t)

	data := []byte("tarball bytes")
	f.writeDone(t, tarball, data)

	// The state a crash between the stub write and the delete leaves behind: a
	// stub, from an earlier attempt and describing other bytes, beside a
	// tarball that is still local. The next sweep re-evicts and the stub must
	// end up describing what was actually evicted.
	_, err := f.store.WriteJSON(store.RemoteStubPath(tarball),
		store.RemoteStub{Version: store.RemoteStubVersion, Size: 999, SHA256: "stale"})
	require.NoError(t, err)

	stats := f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Evicted)

	stub := f.readStub(t, tarball)
	assert.EqualValues(t, len(data), stub.Size)
	assert.Equal(t, manifest.SignatureOf(data).Hash, stub.SHA256)
}

func TestSyncArchiveBackfillsAMissingStub(t *testing.T) {
	t.Parallel()

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t)

	data := []byte("tarball bytes")
	f.writeDone(t, tarball, data)

	stats := f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Evicted)

	// An archive whose tarball was evicted without a stub: it is remote-only
	// and nothing local records it but the ledger.
	require.NoError(t, os.Remove(f.store.AbsPath(store.RemoteStubPath(tarball))))

	stats = f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed, "backfilling a stub is not an eviction or a fault")

	stub := f.readStub(t, tarball)
	assert.EqualValues(t, len(data), stub.Size, "the size comes from the ledger signature")
	assert.Equal(t, manifest.SignatureOf(data).Hash, stub.SHA256)

	_, ok := f.fake.Object(f.key(store.RemoteStubPath(tarball)))
	assert.False(t, ok, "a backfilled stub is no more uploadable than a fresh one")

	// Once every remote-only tarball has a stub the backfill is a no-op, not a
	// rewrite: a sentinel planted here survives, so a converged archive is not
	// churning its own files every sweep.
	f.write(t, store.RemoteStubPath(tarball), []byte("sentinel"))

	stats = f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)

	got, err := os.ReadFile(f.store.AbsPath(store.RemoteStubPath(tarball)))
	require.NoError(t, err)
	assert.Equal(t, "sentinel", string(got), "an existing stub is left alone")
}

func TestSyncArchiveBackfillFailureIsNotASweepFailure(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test makes the write fail with")
	}

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t)

	f.writeDone(t, tarball, []byte("tarball bytes"))

	stats := f.env.SyncArchive(t.Context())
	require.Equal(t, 1, stats.Evicted)

	// A repair the sweep cannot make, unlike an eviction it cannot record, puts
	// no bytes at risk: the mirror holds the tarball and the ledger still proves
	// it, so only the listing stays short. The run must not exit incomplete over
	// it.
	//
	// A read-only config-versions directory is what makes the write fail while
	// the presence probe still answers: traversal survives, creation does not,
	// so the backfill reaches its write and loses.
	require.NoError(t, os.Remove(f.store.AbsPath(store.RemoteStubPath(tarball))))

	dir := f.store.AbsPath(store.ConfigVersionsDirName)
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() {
		require.NoError(t, os.Chmod(dir, 0o700))
	})

	stats = f.env.SyncArchive(t.Context())
	assert.Zero(t, stats.Failed, "a read-model gap is not a custody failure")
	assert.False(t, f.exists(t, store.RemoteStubPath(tarball)),
		"the repair really was attempted and really did fail")
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

func TestSyncArchiveVerifiesEvictedSurfacesRemain(t *testing.T) {
	t.Parallel()

	const (
		tarball = "config-versions/cv-1.tar.gz"
		zip     = "projects/prod/workspaces/api/bundles/logs.gen0001.zip"
	)

	f := newSyncFixture(t)

	f.writeDone(t, tarball, []byte("proven tarball bytes"))
	f.sealBundle(t, zip, "projects/prod/workspaces/api/runs/run-1/plan.log", []byte("plan output"))

	stats := f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)
	require.Equal(t, 2, stats.Evicted)

	// A later sweep against the intact store re-verifies both evicted
	// surfaces from the inventory and stays clean.
	stats = f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed,
		"a store that still holds every evicted surface passes verification")

	// The store loses both only-copies (a re-pointed bucket, a mis-scoped
	// lifecycle rule, a manual delete). No local walk can see the hole, since
	// the zip and tarball have no local files, so the sweep must derive the
	// obligation from the sidecar and the ledger and fail the run.
	require.NoError(t, f.fake.Delete(t.Context(), f.key(tarball)))
	require.NoError(t, f.fake.Delete(t.Context(), f.key(zip)))

	stats = f.env.SyncArchive(t.Context())
	assert.Equal(t, 2, stats.Failed,
		"every evicted surface missing from the store must count a failure")
}

func TestSyncArchiveUnverifiableObligationCountsFailure(t *testing.T) {
	t.Parallel()

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t)

	// A prior sweep evicted the tarball; its done ledger entry is the local
	// side's only record that the store holds the only copy.
	f.writeDone(t, tarball, []byte("proven tarball bytes"))

	stats := f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Evicted)

	// A partial restore leaves a regular file where the config-versions
	// directory was: the local-presence probe now faults (ENOTDIR) instead of
	// answering yes or no, so the only-copy verification cannot run. The
	// fault must count: a run that exits clean here reports a complete mirror
	// over a check that never executed.
	require.NoError(t, os.RemoveAll(f.store.AbsPath("config-versions")))
	f.write(t, "config-versions", []byte("not a directory"))

	stats = f.env.SyncArchive(t.Context())
	assert.Equal(t, 1, stats.Failed,
		"an unverifiable obligation must mark the run incomplete")
}

func TestSyncArchiveVerifiesEvictedTarballSize(t *testing.T) {
	t.Parallel()

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t)

	data := []byte("proven tarball bytes")
	f.writeDone(t, tarball, data)

	stats := f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Evicted)

	// The remote copy is silently replaced with a different-size object; the
	// ledger signature is the local side's only record of the tarball, so the
	// size comparison is what catches it.
	f.fake.SetObject(f.key(tarball), remotetest.Object{Data: []byte("truncated")})

	stats = f.env.SyncArchive(t.Context())
	assert.Equal(t, 1, stats.Failed,
		"an evicted tarball whose remote size diverges from its ledger signature must fail the run")
}

func TestSyncArchiveEvictReadsBackDigestlessCopy(t *testing.T) {
	t.Parallel()

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t)

	data := []byte("proven tarball bytes")
	f.writeDone(t, tarball, data)

	// The store accepts the upload but does not persist its recorded digests
	// (a metadata-dropping S3-compatible endpoint). With nothing egress-free
	// to compare, the confirm must escalate to reading the remote bytes back
	// and hashing them; the archive's only copy is never released on size
	// alone.
	f.fake.HeadHook = func(context.Context) {
		obj, ok := f.fake.Object(f.key(tarball))
		if ok {
			obj.Metadata = nil
			obj.MD5 = nil
			f.fake.SetObject(f.key(tarball), obj)
		}
	}

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Failed)
	assert.Equal(t, 1, stats.Evicted,
		"a read-back that matches the local proof completes the custody transfer")
	assert.False(t, f.exists(t, tarball))
	assert.NotEmpty(t, f.fake.Ranges(),
		"the digestless confirm must actually read the remote bytes back")
}

func TestSyncArchiveEvictReadsBackThroughAResumedDownload(t *testing.T) {
	t.Parallel()

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t, remote.WithRetry(2, 0))

	// Long enough that a mid-body failure lands inside the stream rather than
	// between requests.
	data := []byte(strings.Repeat("proven tarball bytes\n", 5000))
	f.writeDone(t, tarball, data)

	f.fake.HeadHook = func(context.Context) {
		obj, ok := f.fake.Object(f.key(tarball))
		if ok {
			obj.Metadata = nil
			obj.MD5 = nil
			f.fake.SetObject(f.key(tarball), obj)
		}
	}

	// The read-back's body dies partway through. Resuming is what keeps the
	// hash honest: a restart would feed the hasher a duplicated prefix and
	// report a healthy object as a mismatch, blocking its eviction for good.
	f.fake.RangeBodyErr = errors.New("connection reset")
	f.fake.RangeBodyErrAfter = 4096
	f.fake.RangeBodyErrN = 1

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Failed)
	assert.Equal(t, 1, stats.Evicted, "each byte is hashed exactly once across the resume")
	assert.False(t, f.exists(t, tarball))

	ranges := f.fake.Ranges()
	require.Len(t, ranges, 2, "the read-back resumed rather than restarting or giving up")
	assert.Positive(t, ranges[1].Offset, "the second request starts past what already landed")
}

func TestSyncArchiveEvictRefusesDigestlessForeignContent(t *testing.T) {
	t.Parallel()

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t)

	local := []byte("proven tarball bytes")
	foreign := []byte("same-length imposterX")[:len(local)]

	f.writeDone(t, tarball, local)

	// A same-size object with different content and no digests sits at the
	// eviction key (a prior run's upload through a mangling proxy, a foreign
	// write). Size screens pass; only the content read-back can catch it, and
	// it must, or the local delete would leave wrong bytes as the archive's
	// only copy.
	f.fake.SetObject(f.key(tarball), remotetest.Object{Data: foreign})

	stats := f.env.SyncArchive(t.Context())

	assert.Equal(t, 1, stats.Failed)
	assert.Zero(t, stats.Evicted)
	assert.True(t, f.exists(t, tarball),
		"a read-back mismatch keeps the local file canonical")
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

func TestSyncArchiveHistorySidecarSyncsAndSurvivesPrune(t *testing.T) {
	t.Parallel()

	// A history sidecar is a regular archive file: it mirrors under its own
	// key, and while the local file backs it no prune touches the remote
	// copy. Append-only means a stale remote copy is only ever older, so
	// the plain sweep (no eager path) is the right freshness.
	f := newSyncFixture(t)
	st := f.store

	sidecar := st.HistoryPath(st.WorkspaceFile("prod", "api", "variables.json"))
	f.write(t, sidecar, []byte(`{"deleted":true}`+"\n"))

	f.resume()

	stats := f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)

	_, ok := f.fake.Object(f.key(sidecar))
	assert.True(t, ok, "the sidecar mirrors under its own key")

	stats = f.env.SyncArchive(t.Context())
	require.Zero(t, stats.Failed)
	assert.Zero(t, stats.Pruned, "a locally backed sidecar is never pruned")

	_, ok = f.fake.Object(f.key(sidecar))
	assert.True(t, ok, "the remote copy survives the second sweep")
}

func TestSyncArchivePrunesStaleRemoteKeys(t *testing.T) {
	t.Parallel()

	const stale = "projects/prod/workspaces/api/runs/run-1/run.json"

	f := newSyncFixture(t)
	f.resume()

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

	// The evicted surfaces' local proof is gone: the sidecar lost with a
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

func TestSyncArchiveFreshLedgerRefusesPrune(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	// The run opened an empty ledger (a fresh or wrong --output) while the
	// mirror already holds history. Pruning would delete the mirror's only
	// record of it, so the sweep must refuse, and refuse loudly: the run
	// exits incomplete so a scheduled run cannot silently diverge the mirror.
	f.fake.SetObject(f.key("projects/old/workspaces/gone/workspace.json"),
		remotetest.Object{Data: []byte("history")})
	f.write(t, "org.json", []byte(`{"org":"acme"}`))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Pruned)
	assert.Empty(t, f.fake.Deleted())
	assert.Equal(t, 1, stats.Failed, "a refused prune must mark the run incomplete")
}

func TestSyncArchivePruneRefusesMassDeletion(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.resume()

	// A resumed run whose local tree lost most of its files (partial disk
	// loss, a botched restore): the delete set outnumbers everything the walk
	// matched, which no steady-state re-shape produces. The mirror is the
	// disaster-recovery copy, so a mass deletion must be refused and surfaced.
	for i := range 150 {
		f.fake.SetObject(f.key(fmt.Sprintf("projects/p/workspaces/w/runs/run-%03d/run.json", i)),
			remotetest.Object{Data: []byte("history")})
	}

	f.write(t, "org.json", []byte(`{"org":"acme"}`))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Pruned)
	assert.Empty(t, f.fake.Deleted())
	assert.Equal(t, 1, stats.Failed, "a refused prune must mark the run incomplete")
}

func TestSyncArchivePruneAllowsMassReshape(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.resume()

	// The tool's own self-heal produces a mass re-shape: an interrupted run
	// mirrored hundreds of loose run files, the next run coalesced them all
	// into the workspace's roll-up. The ledger holds every entry and the
	// coalesced form sits in the workspace where the loose sources used to be,
	// so the disproportion guard must not mistake the re-shape for loss, or the
	// refusal would repeat every run forever on a healthy archive.
	for i := range 150 {
		relPath := fmt.Sprintf("projects/p/workspaces/w/runs/run-%03d/run.json", i)
		f.fake.SetObject(f.key(relPath), remotetest.Object{Data: []byte("loose copy")})
		f.ledger.RecordDone(relPath, manifest.SignatureOf([]byte("loose copy")))
	}

	f.write(t, "projects/p/workspaces/w/rollups/runs.ndjson", []byte(`{"path":"runs/run-000/run.json"}`))
	f.write(t, "org.json", []byte(`{"org":"acme"}`))

	stats := f.env.SyncArchive(t.Context())

	assert.Equal(t, 150, stats.Pruned,
		"a seal's own artifacts explain the stale keys, which prune at any scale")
	assert.Zero(t, stats.Failed)
}

func TestSyncArchivePruneRefusesMassLossUnderASurvivingLedger(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.resume()

	// A partial restore brought the ledger back but not the data files it
	// describes. Every mirrored key is now ledger-known and locally absent,
	// which the ledger alone cannot tell from a re-shape, but nothing local
	// says a seal ever moved them: the workspace holds no bundle and no
	// roll-up. The mirror is the only copy of the search layer at this point,
	// so the sweep must refuse rather than delete it and let the run report a
	// complete mirror over the hole.
	for i := range 150 {
		relPath := fmt.Sprintf("projects/p/workspaces/w/runs/run-%03d/run.json", i)
		f.fake.SetObject(f.key(relPath), remotetest.Object{Data: []byte("only copy")})
		f.ledger.RecordDone(relPath, manifest.SignatureOf([]byte("only copy")))
	}

	f.write(t, "org.json", []byte(`{"org":"acme"}`))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Pruned)
	assert.Empty(t, f.fake.Deleted())
	assert.Equal(t, 1, stats.Failed, "a refused prune must mark the run incomplete")

	for i := range 150 {
		_, present := f.fake.Object(f.key(fmt.Sprintf("projects/p/workspaces/w/runs/run-%03d/run.json", i)))
		require.True(t, present, "the mirror's only copies survive the refusal")
	}
}

func TestSyncArchivePruneRefusalIsScopedToTheUnbackedWorkspace(t *testing.T) {
	t.Parallel()

	const sealed = "projects/p/workspaces/sealed/runs/run-1/run.json"

	f := newSyncFixture(t)
	f.resume()

	// One workspace sealed its runs away and another lost its files with its
	// ledger entries intact. The refusal covers the unbacked keys only: the
	// sealed workspace's stale loose copy still prunes, so a mass loss in one
	// corner of the archive does not freeze the mirror's convergence
	// everywhere else.
	f.fake.SetObject(f.key(sealed), remotetest.Object{Data: []byte("loose copy")})
	f.ledger.RecordDone(sealed, manifest.SignatureOf([]byte("loose copy")))
	f.write(t, "projects/p/workspaces/sealed/rollups/runs.ndjson", []byte(`{"path":"x"}`))

	for i := range 150 {
		relPath := fmt.Sprintf("projects/p/workspaces/lost/runs/run-%03d/run.json", i)
		f.fake.SetObject(f.key(relPath), remotetest.Object{Data: []byte("only copy")})
		f.ledger.RecordDone(relPath, manifest.SignatureOf([]byte("only copy")))
	}

	stats := f.env.SyncArchive(t.Context())

	assert.Equal(t, 1, stats.Pruned)
	assert.Equal(t, []string{f.key(sealed)}, f.fake.Deleted())
	assert.Equal(t, 1, stats.Failed, "a refused prune must mark the run incomplete")
}

func TestSyncArchivePrunesUnderARenameAlias(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.resume()

	// An operator moved a workspace subtree and left a rename symlink behind:
	// the walk declines the alias and reports every file under the owner's
	// name, so the mirror's alias-named keys are stale in bulk. The walk's own
	// alias pairs explain them (the bytes are local, spelled differently), so
	// the disproportion guard must not read the rename as loss.
	f.writeDone(t, "projects/prod/workspaces/api/workspace.json", []byte(`{"ws":"api"}`))

	wsDir := filepath.Join(f.store.Root(), "projects", "prod", "workspaces")
	require.NoError(t, os.Symlink(filepath.Join(wsDir, "api"), filepath.Join(wsDir, "renamed")))

	for i := range 150 {
		relPath := fmt.Sprintf("projects/prod/workspaces/renamed/runs/run-%03d/run.json", i)
		f.fake.SetObject(f.key(relPath), remotetest.Object{Data: []byte("old name")})
		f.ledger.RecordDone(relPath, manifest.SignatureOf([]byte("old name")))
	}

	stats := f.env.SyncArchive(t.Context())

	assert.Equal(t, 150, stats.Pruned, "a rename alias explains its stale keys at any scale")
	assert.Zero(t, stats.Failed)
}

func TestSyncArchivePruneExemptsBundleSidecars(t *testing.T) {
	t.Parallel()

	const sidecar = "projects/prod/workspaces/api/bundles/logs.gen0001.zip.sidecar.ndjson"

	f := newSyncFixture(t)
	f.resume()

	// The workspace subtree was lost locally, taking the sidecar with it. The
	// mirrored sidecar is now the only index proving the remote-only zip's
	// members, so the prune must leave it: the zip it proves is exempt by
	// shape, and a proof without its subject is worthless the other way too.
	f.fake.SetObject(f.key(sidecar), remotetest.Object{Data: []byte(`{"name":"x"}`)})
	f.write(t, "org.json", []byte(`{"org":"acme"}`))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Pruned)
	assert.Empty(t, f.fake.Deleted())
	assert.Zero(t, stats.Failed)
}

func TestSyncArchivePrunesKeysTheMirrorMustNotHold(t *testing.T) {
	t.Parallel()

	// A mirror seeded out of band (an aws s3 sync of a local archive) holds
	// keys this tool never uploads. The local file of the same name is not
	// their backing: it is the ledger's own log or flock target, a staging
	// temp, an eviction stub. The presence fence must not protect them.
	// The replay log is why this matters: restoring a mirror that kept one
	// would replay stale ledger state over the newer snapshot beside it.
	tests := map[string]struct {
		relPath string
		want    bool
	}{
		"the ledger's replay log": {
			relPath: ".ledger/log.ndjson",
			want:    true,
		},
		"a shard's replay log": {
			relPath: "projects/prod/workspaces/api/.ledger/log.ndjson",
			want:    true,
		},
		"the ledger's flock target": {
			relPath: ".ledger/lock",
			want:    true,
		},
		"a staging temp a crash left behind": {
			relPath: "projects/prod/workspaces/api/.atomicfile-123.tmp",
			want:    true,
		},
		"an eviction stub, which points at the mirror from outside it": {
			relPath: "config-versions/cv-1.tar.gz" + store.RemoteStubSuffix,
			want:    true,
		},
		"the ledger snapshot, which the mirror does hold": {
			relPath: ".ledger/snapshot.json",
			want:    false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newSyncFixture(t)
			f.resume()

			f.write(t, tc.relPath, []byte("local copy"))
			f.write(t, "org.json", []byte(`{"org":"acme"}`))
			f.fake.SetObject(f.key(tc.relPath), remotetest.Object{Data: []byte("seeded copy")})

			stats := f.env.SyncArchive(t.Context())
			require.Zero(t, stats.Failed)

			_, present := f.fake.Object(f.key(tc.relPath))

			if tc.want {
				assert.Equal(t, 1, stats.Pruned)
				assert.False(t, present, "a key the mirror must not hold prunes past its local file")

				return
			}

			assert.Zero(t, stats.Pruned)
			assert.True(t, present, "a mirrored file's local copy still protects its key")
		})
	}
}

func TestSyncArchiveFollowsSymlinkedDirectories(t *testing.T) {
	t.Parallel()

	const (
		wsFile  = "projects/prod/workspaces/api/workspace.json"
		runFile = "projects/prod/workspaces/api/runs/run-1/run.json"
	)

	f := newSyncFixture(t)
	f.resume()

	// The mirrored state of a prior run: both workspace files uploaded and
	// recorded in the ledger.
	for _, relPath := range []string{wsFile, runFile} {
		data := []byte(`{"path":"` + relPath + `"}`)
		f.writeDone(t, relPath, data)
		f.fake.SetObject(f.key(relPath),
			remotetest.Object{Data: data, MD5: remotetest.MD5Sum(data)})
	}

	// The operator relocates projects/ to another volume and leaves a symlink,
	// the layout ledger shard discovery already follows. The sweep must see
	// the same tree the ledger describes, or every mirrored key beneath the
	// link reads as stale and the prune mass-deletes live mirror content.
	root := f.store.Root()
	relocated := filepath.Join(t.TempDir(), "relocated")
	require.NoError(t, os.Rename(filepath.Join(root, "projects"), relocated))
	require.NoError(t, os.Symlink(relocated, filepath.Join(root, "projects")))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Failed)
	assert.Zero(t, stats.Pruned, "a relocated subtree's mirrored keys must not read as stale")
	assert.Empty(t, f.fake.Deleted())
	assert.Equal(t, 2, stats.Skipped, "the walk reaches both mirrored files through the link")
}

func TestSyncArchiveSymlinkLeafShapes(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	// A symlink to a regular file mirrors like the file; a dangling link
	// reads as absent, not as a walk error that would abort the sweep.
	data := []byte(`{"org":"acme"}`)
	target := filepath.Join(t.TempDir(), "org.json")
	require.NoError(t, os.WriteFile(target, data, 0o600))

	root := f.store.Root()
	require.NoError(t, os.Symlink(target, filepath.Join(root, "org.json")))
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone"),
		filepath.Join(root, "dangling.json")))

	stats := f.env.SyncArchive(t.Context())

	require.Zero(t, stats.Failed)
	assert.Equal(t, 1, stats.Uploaded)

	obj, ok := f.fake.Object(f.key("org.json"))
	require.True(t, ok, "a symlinked file mirrors at the link's archive path")
	assert.Equal(t, data, obj.Data)
}

func TestSyncSubtreeFollowsSymlinkedWorkspace(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.write(t, wsSubtree+"/workspace.json", []byte(`{"ws":"api"}`))

	// The workspace directory itself is relocated behind a symlink: the walk
	// root is then a link, which the seal-boundary sync must still read
	// through or the subtree never mirrors at all.
	root := f.store.Root()
	target := filepath.Join(t.TempDir(), "api")
	require.NoError(t, os.Rename(filepath.Join(root, filepath.FromSlash(wsSubtree)), target))
	require.NoError(t, os.Symlink(target, filepath.Join(root, filepath.FromSlash(wsSubtree))))

	stats := f.env.SyncSubtree(t.Context(), wsSubtree)

	require.Zero(t, stats.Failed)
	assert.Equal(t, 1, stats.Uploaded, "a leaf-symlinked workspace still mirrors at its seal boundary")

	_, ok := f.fake.Object(f.key(wsSubtree + "/workspace.json"))
	assert.True(t, ok)
}

func TestSyncArchiveWalksSiblingAliasedSubtreeOnce(t *testing.T) {
	t.Parallel()

	const wsFile = "projects/prod/workspaces/api/workspace.json"

	f := newSyncFixture(t)
	f.resume()

	data := []byte(`{"ws":"api"}`)
	f.writeDone(t, wsFile, data)
	f.fake.SetObject(f.key(wsFile),
		remotetest.Object{Data: data, MD5: remotetest.MD5Sum(data)})

	// A rename symlink an operator left beside its target: the same physical
	// workspace reachable under two sibling names. The sweep must walk the
	// subtree once: walked under both names, its files would sync, evict, and
	// count twice, here a duplicate upload of the same content under the
	// alias's remote key.
	wsDir := filepath.Join(f.store.Root(), "projects", "prod", "workspaces")
	require.NoError(t, os.Symlink(
		filepath.Join(wsDir, "api"),
		filepath.Join(wsDir, "renamed"),
	))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Failed)
	assert.Zero(t, stats.Pruned)
	assert.Equal(t, 1, stats.Skipped, "the aliased subtree walks once")
	assert.Zero(t, stats.Uploaded, "no duplicate upload under the alias key")

	_, ok := f.fake.Object(f.key("projects/prod/workspaces/renamed/workspace.json"))
	assert.False(t, ok, "the alias name mirrors nothing of its own")
}

func TestSyncArchiveAliasSortingFirstDoesNotShadowTheRealName(t *testing.T) {
	t.Parallel()

	const wsFile = "projects/prod/workspaces/api/workspace.json"

	f := newSyncFixture(t)
	f.resume()

	data := []byte(`{"ws":"api"}`)
	f.writeDone(t, wsFile, data)
	f.fake.SetObject(f.key(wsFile),
		remotetest.Object{Data: data, MD5: remotetest.MD5Sum(data)})

	// A rename alias that sorts lexically before its target. The walk must
	// still report the subtree under the physical name the ledger and remote
	// keys use: a walk that let the alias claim it would fill the keep set
	// with alias keys only, and the prune would delete the live mirror of
	// every real-name key while the sweep re-uploaded it all under the alias,
	// a cycle that repeats every run.
	wsDir := filepath.Join(f.store.Root(), "projects", "prod", "workspaces")
	require.NoError(t, os.Symlink(
		filepath.Join(wsDir, "api"),
		filepath.Join(wsDir, "aaa"),
	))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Failed)
	assert.Zero(t, stats.Pruned, "the real-name mirror survives")
	assert.Empty(t, f.fake.Deleted())
	assert.Zero(t, stats.Uploaded, "nothing re-uploads under the alias name")
	assert.Equal(t, 1, stats.Skipped, "the mirrored file matches under its real name")

	_, ok := f.fake.Object(f.key("projects/prod/workspaces/aaa/workspace.json"))
	assert.False(t, ok)
}

func TestSyncArchiveHealsAnEvictedZipOntoItsCurrentKey(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.resume()

	// An eviction that ran before a workspace rename: the sidecar walks under
	// the physical name today, but the zip's only copy sits at the key the
	// old name minted. The rename alias the walk declines must bridge the
	// lookup, and the credit must then converge the mirror (a server-side copy
	// onto the current key, the historical key released behind the confirmed
	// copy), so the archive stops depending on the rename link surviving on
	// disk.
	const (
		sidecar = "projects/prod/workspaces/api/bundles/logs.gen0001.zip.sidecar.ndjson"
		curZip  = "projects/prod/workspaces/api/bundles/logs.gen0001.zip"
		oldZip  = "projects/prod/workspaces/zeta-old/bundles/logs.gen0001.zip"
	)

	f.write(t, sidecar, []byte(`{"name":"member.json"}`))

	// The eviction that wrote the historical copy recorded its digest, the
	// proof the heal's confirm compares the fresh copy against.
	f.fake.SetObject(f.key(oldZip), remotetest.Object{
		Data: []byte("zipbytes"),
		MD5:  remotetest.MD5Sum([]byte("zipbytes")),
	})

	wsDir := filepath.Join(f.store.Root(), "projects", "prod", "workspaces")
	link := filepath.Join(wsDir, "zeta-old")
	require.NoError(t, os.Symlink(filepath.Join(wsDir, "api"), link))

	stats := f.env.SyncArchive(t.Context())

	require.Zero(t, stats.Failed, "the historical key satisfies the only-copy obligation")

	healed, ok := f.fake.Object(f.key(curZip))
	require.True(t, ok, "the only-copy converges onto the current key")
	assert.Equal(t, []byte("zipbytes"), healed.Data)
	assert.Equal(t, []string{f.key(oldZip)}, f.fake.Deleted(),
		"the historical key is released behind the confirmed copy")

	// With the mirror converged, the rename link is no longer load-bearing:
	// a sweep without it satisfies the obligation at the current key.
	require.NoError(t, os.Remove(link))

	stats = f.env.SyncArchive(t.Context())
	assert.Zero(t, stats.Failed, "the healed mirror no longer needs the alias")
}

func TestSyncArchiveKeepsTheHistoricalCopyWhenTheHealFails(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.resume()

	// A failed heal must not disturb custody: the credit already succeeded,
	// the historical copy stays the only copy, and the run stays clean while
	// the next sweep retries the heal.
	const (
		sidecar = "projects/prod/workspaces/api/bundles/logs.gen0001.zip.sidecar.ndjson"
		oldZip  = "projects/prod/workspaces/zeta-old/bundles/logs.gen0001.zip"
	)

	f.write(t, sidecar, []byte(`{"name":"member.json"}`))
	f.fake.SetObject(f.key(oldZip), remotetest.Object{
		Data: []byte("zipbytes"),
		MD5:  remotetest.MD5Sum([]byte("zipbytes")),
	})

	f.fake.CopyErr = errors.New("copy boom")

	wsDir := filepath.Join(f.store.Root(), "projects", "prod", "workspaces")
	require.NoError(t, os.Symlink(
		filepath.Join(wsDir, "api"),
		filepath.Join(wsDir, "zeta-old"),
	))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Failed, "the credit stands; the heal retries next sweep")
	assert.Empty(t, f.fake.Deleted(), "the only copy is never released without its replacement")

	_, ok := f.fake.Object(f.key(oldZip))
	assert.True(t, ok)
}

func TestSyncArchiveHealConfirmsTheCopyBeforeReleasingIt(t *testing.T) {
	t.Parallel()

	const (
		tarball = "config-versions/sub/cv-1.tar.gz"
		histRel = "config-versions/old/cv-1.tar.gz"
		content = "tarball bytes"
	)

	// The historical copy predates recorded digest metadata (an older build's
	// upload, or a store that dropped it), so the copy onto the current key
	// streams with nothing verifying it at commit. Size is all the sweep can
	// see from the listing, and size is what same-size rot preserves, so the
	// confirm reads the fresh copy back and hashes it against the signature
	// that settled the tarball before the archive's other copy is released.
	tests := map[string]struct {
		stored string
		want   bool
	}{
		"a copy that hashes to the record is released": {
			stored: content,
			want:   true,
		},
		"a same-size copy that hashes to something else is not": {
			stored: "tarball bytez",
			want:   false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newSyncFixture(t)
			f.resume()

			// An unproven neighbor keeps the tarball's directory on disk, so
			// the rename link beside it is one the walk declines rather than a
			// dangling symlink.
			f.write(t, "config-versions/sub/cv-2.tar.gz", []byte("neighbor"))

			cvDir := filepath.Join(f.store.Root(), "config-versions")
			require.NoError(t, os.Symlink(
				filepath.Join(cvDir, "sub"),
				filepath.Join(cvDir, "old"),
			))

			// The tarball is evicted: its ledger record is the local side's
			// only proof, and the mirror holds its bytes under the pre-rename
			// name.
			f.ledger.RecordDone(tarball, manifest.SignatureOf([]byte(content)))
			require.NoError(t, f.ledger.Flush())
			f.fake.SetObject(f.key(histRel), remotetest.Object{Data: []byte(tc.stored)})

			stats := f.env.SyncArchive(t.Context())
			require.Zero(t, stats.Failed, "the historical key satisfies the only-copy obligation")

			historical, ok := f.fake.Object(f.key(histRel))
			healed, healedOK := f.fake.Object(f.key(tarball))

			if tc.want {
				assert.False(t, ok, "the historical copy is released behind the proven one")
				require.True(t, healedOK, "the only-copy converges onto the current key")
				assert.Equal(t, []byte(content), healed.Data)

				return
			}

			require.True(t, ok, "an unproven copy never releases the archive's other one")
			assert.Equal(t, []byte(tc.stored), historical.Data)
			assert.False(t, healedOK,
				"the refused copy is removed, or the next sweep would credit the obligation at it")
		})
	}
}

func TestSyncArchiveHealLeavesACopyNothingCanProve(t *testing.T) {
	t.Parallel()

	const (
		sidecar = "projects/prod/workspaces/api/bundles/logs.gen0001.zip.sidecar.ndjson"
		curZip  = "projects/prod/workspaces/api/bundles/logs.gen0001.zip"
		oldZip  = "projects/prod/workspaces/zeta-old/bundles/logs.gen0001.zip"
	)

	f := newSyncFixture(t)
	f.resume()

	// A digestless zip: the store recorded none for the object, and a bundle's
	// sidecar records its members' digests rather than the zip's, so no proof
	// exists that a copy onto the current key carries these bytes. Converging
	// anyway would release the archive's only copy on size, so the sweep
	// copies nothing and keeps living off the rename link.
	f.write(t, sidecar, []byte(`{"name":"member.json"}`))
	f.fake.SetObject(f.key(oldZip), remotetest.Object{Data: []byte("zipbytes")})

	wsDir := filepath.Join(f.store.Root(), "projects", "prod", "workspaces")
	require.NoError(t, os.Symlink(
		filepath.Join(wsDir, "api"),
		filepath.Join(wsDir, "zeta-old"),
	))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Failed, "the credit stands whether or not the heal can converge")
	assert.Empty(t, f.fake.Copies(), "an unprovable heal is never paid for")
	assert.Empty(t, f.fake.Deleted())

	_, ok := f.fake.Object(f.key(oldZip))
	assert.True(t, ok, "the only copy stays where the credit found it")

	_, ok = f.fake.Object(f.key(curZip))
	assert.False(t, ok)
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
	assert.Zero(t, f.env.RemoteTally().Failed,
		"a cancellation surfacing from inside the offload settles nothing run-wide either")
}

func TestSyncArchivePruneCancellationIsNotFailure(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.resume()

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

func TestUnderWorkspaceSubtree(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		relPath string
		want    bool
	}{
		"a workspace file": {
			relPath: "projects/prod/workspaces/api/workspace.json",
			want:    true,
		},
		"a nested run child": {
			relPath: "projects/prod/workspaces/api/runs/run-1/run.json",
			want:    true,
		},
		"the workspace directory itself has no file segment": {
			relPath: "projects/prod/workspaces/api",
		},
		"a project file sits above the workspaces": {
			relPath: "projects/prod/project.json",
		},
		"a stack subtree is not a workspace subtree": {
			relPath: "projects/prod/stacks/net/stack.json",
		},
		"an org-root file": {
			relPath: "org.json",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, collect.UnderWorkspaceSubtree(tc.relPath))
		})
	}
}

func TestEagerScope(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		relPath string
		want    bool
	}{
		"an org metadata file syncs as written": {
			relPath: "org.json",
			want:    true,
		},
		"a user file syncs as written": {
			relPath: "users/user-1.json",
			want:    true,
		},
		"a project file syncs as written": {
			relPath: "projects/prod/project.json",
			want:    true,
		},
		"a stack file syncs as written (stacks have no seal path)": {
			relPath: "projects/prod/stacks/net/deployments/production/deployment.json",
			want:    true,
		},
		"a workspace-subtree file waits for its seal boundary": {
			relPath: "projects/prod/workspaces/api/runs/run-1/plan.log",
		},
		"a configuration-version tarball moves by eviction, not sync": {
			relPath: "config-versions/cv-1.tar.gz",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, collect.EagerScope(tc.relPath))
		})
	}
}

// wsSubtree is the workspace subtree the SyncSubtree tests mirror.
const wsSubtree = "projects/prod/workspaces/api"

func TestSyncSubtreeNoRemoteIsNoop(t *testing.T) {
	t.Parallel()

	env, _, _ := newEnv(t)

	assert.Zero(t, env.SyncSubtree(t.Context(), wsSubtree))
}

func TestSyncSubtreeMirrorsSearchLayer(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	// The subtree's search layer after a seal: loose metadata, a roll-up, and
	// a sidecar index; beside them an orphan zip (crash mid-seal, never
	// uploaded) and the workspace shard's ledger files (mid-run state the
	// close sweep mirrors post-compaction).
	f.write(t, wsSubtree+"/workspace.json", []byte(`{"ws":"api"}`))
	f.write(t, wsSubtree+"/rollups/runs.ndjson", []byte(`{"id":"run-1"}`))
	f.write(t, wsSubtree+"/bundles/logs.gen0001.zip.sidecar.ndjson", []byte(`{"name":"x"}`))
	f.write(t, wsSubtree+"/bundles/logs.gen0002.zip", []byte("orphan zip"))
	f.write(t, wsSubtree+"/"+manifest.LedgerDirName+"/snapshot.json", []byte(`{}`))
	f.write(t, wsSubtree+"/"+manifest.LedgerDirName+"/"+manifest.LogFileName, []byte(`{}`))

	// A file outside the subtree is out of scope entirely.
	f.write(t, "org.json", []byte(`{"org":"acme"}`))

	stats := f.env.SyncSubtree(t.Context(), wsSubtree)

	require.Zero(t, stats.Failed)
	assert.Equal(t, 3, stats.Uploaded)
	assert.Equal(t, []string{
		f.key(wsSubtree + "/bundles/logs.gen0001.zip.sidecar.ndjson"),
		f.key(wsSubtree + "/rollups/runs.ndjson"),
		f.key(wsSubtree + "/workspace.json"),
	}, f.fake.Keys(), "exactly the subtree's search layer is mirrored")
}

func TestSyncSubtreeMissingSubtreeIsNoop(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	stats := f.env.SyncSubtree(t.Context(), wsSubtree)

	assert.Zero(t, stats, "a subtree that never materialized has nothing to mirror")
	assert.Zero(t, f.env.EagerFailures())
}

func TestSyncSubtreeIncrementalGate(t *testing.T) {
	t.Parallel()

	relPath := wsSubtree + "/workspace.json"
	content := []byte(`{"ws":"api"}`)

	tests := map[string]struct {
		remoteObj    *remotetest.Object
		wantUploaded int
		wantSkipped  int
	}{
		"an absent key uploads": {
			wantUploaded: 1,
		},
		"a stale copy uploads": {
			remoteObj:    &remotetest.Object{Data: []byte("longer stale copy")},
			wantUploaded: 1,
		},
		"a matching copy skips": {
			remoteObj:   &remotetest.Object{Data: content, MD5: remotetest.MD5Sum(content)},
			wantSkipped: 1,
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

			stats := f.env.SyncSubtree(t.Context(), wsSubtree)

			require.Zero(t, stats.Failed)
			assert.Equal(t, tc.wantUploaded, stats.Uploaded)
			assert.Equal(t, tc.wantSkipped, stats.Skipped)

			obj, ok := f.fake.Object(f.key(relPath))
			require.True(t, ok)
			assert.Equal(t, content, obj.Data)
		})
	}
}

func TestSyncSubtreeNeverPrunes(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	// A stale remote key nothing local backs anymore: deletion reconciliation
	// stays global at the close sweep, so the subtree sync must leave it.
	stale := wsSubtree + "/runs/run-1/run.json"
	f.fake.SetObject(f.key(stale), remotetest.Object{Data: []byte(`{"id":"run-1"}`)})

	f.write(t, wsSubtree+"/rollups/runs.ndjson", []byte(`{"id":"run-1"}`))

	stats := f.env.SyncSubtree(t.Context(), wsSubtree)

	require.Zero(t, stats.Failed)
	assert.Zero(t, stats.Pruned)
	assert.Empty(t, f.fake.Deleted())

	_, ok := f.fake.Object(f.key(stale))
	assert.True(t, ok, "the stale key waits for the close sweep's prune")
}

func TestSyncSubtreeCanceledContextSettlesNothing(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.write(t, wsSubtree+"/workspace.json", []byte(`{"ws":"api"}`))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	stats := f.env.SyncSubtree(ctx, wsSubtree)

	assert.Zero(t, stats, "a cancellation is the wind-down, not a failure")
	assert.Empty(t, f.fake.Keys())
	assert.Zero(t, f.env.EagerFailures())
}

func TestSyncSubtreeFailureCountsAndDefersToSweep(t *testing.T) {
	t.Parallel()

	relPath := wsSubtree + "/workspace.json"

	f := newSyncFixture(t)
	f.write(t, relPath, []byte(`{"ws":"api"}`))

	f.fake.PutErr = assert.AnError
	f.fake.PutErrN = 1

	stats := f.env.SyncSubtree(t.Context(), wsSubtree)

	assert.Equal(t, 1, stats.Failed)
	assert.Equal(t, 1, f.env.EagerFailures(), "a subtree failure counts as an eager failure")

	sweep := f.env.SyncArchive(t.Context())

	assert.Zero(t, sweep.Failed, "the sweep's Failed reflects only sweep outcomes")
	assert.Equal(t, 1, sweep.Uploaded, "the close sweep retries the eager failure")

	_, ok := f.fake.Object(f.key(relPath))
	assert.True(t, ok)
}

func TestEagerSyncMirrorsOrgScopeWriteAsWritten(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	data := []byte(`{"org":"acme"}`)

	require.NoError(t, f.env.Bytes(t.Context(), "org.json",
		func(context.Context) ([]byte, error) { return data, nil }))

	obj, ok := f.fake.Object(f.key("org.json"))
	require.True(t, ok, "an org-scope write mirrors the moment it commits")
	assert.Equal(t, data, obj.Data)
}

func TestEagerSyncSkipsUnchangedRewrite(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	fetch := func(context.Context) (any, error) {
		return map[string]string{"org": "acme"}, nil
	}

	require.NoError(t, f.env.Mutable(t.Context(), "org.json", fetch))
	require.Equal(t, 1, f.fake.PutCalls())

	require.NoError(t, f.env.Mutable(t.Context(), "org.json", fetch))
	assert.Equal(t, 1, f.fake.PutCalls(), "a re-read that changed nothing on disk uploads nothing")
}

func TestEagerSyncSkipsWorkspaceSubtreeWrites(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	require.NoError(t, f.env.Bytes(t.Context(), wsSubtree+"/runs/run-1/plan.log",
		func(context.Context) ([]byte, error) { return []byte("plan output"), nil }))

	assert.Empty(t, f.fake.Keys(),
		"a seal-destined file waits for its workspace's seal boundary")
}

func TestEagerSyncFailureDefersToSweep(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	f.fake.PutErr = assert.AnError
	f.fake.PutErrN = 1

	require.NoError(t, f.env.Bytes(t.Context(), "org.json",
		func(context.Context) ([]byte, error) { return []byte(`{"org":"acme"}`), nil }),
		"an eager failure never fails the write it rides on")

	assert.Equal(t, 1, f.env.EagerFailures())

	_, ok := f.fake.Object(f.key("org.json"))
	require.False(t, ok)

	sweep := f.env.SyncArchive(t.Context())

	assert.Zero(t, sweep.Failed, "the sweep's Failed reflects only sweep outcomes")
	assert.Equal(t, 1, sweep.Uploaded)

	_, ok = f.fake.Object(f.key("org.json"))
	assert.True(t, ok)
}

func TestEagerSyncBoundsConcurrentUploads(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	// Track the high-water mark of concurrent commits: the per-page archive
	// fan-out is unbounded, so the eager semaphore is the only cap.
	var inFlight, peak atomic.Int64

	f.fake.PutHook = func(context.Context) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)

		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}

		time.Sleep(time.Millisecond)
	}

	const writes = 4 * collect.DefaultConcurrency

	var g errgroup.Group

	for i := range writes {
		relPath := fmt.Sprintf("users/user-%02d.json", i)

		g.Go(func() error {
			return f.env.Bytes(t.Context(), relPath,
				func(context.Context) ([]byte, error) { return []byte(`{"id":"` + relPath + `"}`), nil })
		})
	}

	require.NoError(t, g.Wait())

	assert.LessOrEqual(t, peak.Load(), int64(collect.DefaultConcurrency),
		"in-flight eager uploads never exceed the fan-out ceiling")
	assert.Len(t, f.fake.Keys(), writes, "every write still mirrors")
}

// TestRemoteTallySpansMotions drives all four remote motions through one
// environment and asserts the run-wide tally accumulates across them while
// each pass's own [collect.SyncStats] stays scoped to that pass, including
// where the two split: eviction bytes ride the run tally but never a pass's
// UploadedBytes.
func TestRemoteTallySpansMotions(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	// Motion one: an eager as-written upload the moment the write commits.
	orgData := []byte(`{"org":"acme"}`)
	require.NoError(t, f.env.Bytes(t.Context(), "org.json",
		func(context.Context) ([]byte, error) { return orgData, nil }))

	tally := f.env.RemoteTally()
	assert.Equal(t, 1, tally.Uploaded)
	assert.Equal(t, int64(len(orgData)), tally.UploadedBytes)

	// Motion two: the seal-boundary subtree sync.
	wsData := []byte(`{"ws":"api"}`)
	f.write(t, wsSubtree+"/workspace.json", wsData)

	subtree := f.env.SyncSubtree(t.Context(), wsSubtree)
	require.Zero(t, subtree.Failed)

	tally = f.env.RemoteTally()
	assert.Equal(t, 2, tally.Uploaded)
	assert.Equal(t, int64(len(orgData)+len(wsData)), tally.UploadedBytes)

	// Motions three and four: the close sweep syncs the sidecar and evicts the
	// sealed bundle. The already-mirrored files skip and count nothing.
	zip := wsSubtree + "/bundles/logs.gen0001.zip"
	f.sealBundle(t, zip, "projects/prod/workspaces/api/runs/run-1/plan.log", []byte("plan output"))

	zipInfo, err := os.Stat(f.store.AbsPath(zip))
	require.NoError(t, err)

	sidecarInfo, err := os.Stat(f.store.AbsPath(zip + seal.SidecarSuffix))
	require.NoError(t, err)

	sweep := f.env.SyncArchive(t.Context())
	require.Zero(t, sweep.Failed)
	require.Equal(t, 1, sweep.Evicted)
	assert.Equal(t, 1, sweep.Uploaded, "the sweep uploads only the sidecar")
	assert.Equal(t, sidecarInfo.Size(), sweep.UploadedBytes,
		"the pass's uploaded bytes exclude the eviction transfer")

	tally = f.env.RemoteTally()
	assert.Equal(t, 3, tally.Uploaded)
	assert.Equal(t, 1, tally.Evicted)
	assert.Zero(t, tally.Failed)
	assert.Equal(t, int64(len(orgData)+len(wsData))+sidecarInfo.Size()+zipInfo.Size(),
		tally.UploadedBytes, "the run tally includes the eviction transfer")
}

func TestRemoteTallyFailureMotions(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		run func(t *testing.T, f syncFixture)
	}{
		"an eager upload failure": {
			run: func(t *testing.T, f syncFixture) {
				t.Helper()

				f.fake.PutErr = assert.AnError
				f.fake.PutErrN = 1

				require.NoError(t, f.env.Bytes(t.Context(), "org.json",
					func(context.Context) ([]byte, error) { return []byte(`{"org":"acme"}`), nil }))
			},
		},
		"a subtree sync failure": {
			run: func(t *testing.T, f syncFixture) {
				t.Helper()

				f.write(t, wsSubtree+"/workspace.json", []byte(`{"ws":"api"}`))

				f.fake.PutErr = assert.AnError
				f.fake.PutErrN = 1

				stats := f.env.SyncSubtree(t.Context(), wsSubtree)
				require.Equal(t, 1, stats.Failed)
			},
		},
		"an eviction transfer failure": {
			run: func(t *testing.T, f syncFixture) {
				t.Helper()

				f.writeDone(t, "config-versions/cv-1.tar.gz", []byte("tarball bytes"))

				f.fake.PutErr = assert.AnError
				f.fake.PutErrN = 1

				stats := f.env.SyncArchive(t.Context())
				require.Equal(t, 1, stats.Failed)
			},
		},
		"an eviction refused before any transfer": {
			run: func(t *testing.T, f syncFixture) {
				t.Helper()

				// The proof gate refuses rotted bytes with no remote traffic at
				// all; the refusal still counts, matching the pass's Failed.
				f.writeDone(t, "config-versions/cv-1.tar.gz", []byte("proven tarball bytes"))
				f.write(t, "config-versions/cv-1.tar.gz", []byte("rotted tarball bytes"))

				stats := f.env.SyncArchive(t.Context())
				require.Equal(t, 1, stats.Failed)
			},
		},
		"a suspect tarball caught by the size screen": {
			run: func(t *testing.T, f syncFixture) {
				t.Helper()

				// Rot that changed the byte count is caught one step earlier
				// than the proof gate, by the classification's stat screen; it
				// must weigh the same as rot the proof gate catches.
				f.writeDone(t, "config-versions/cv-1.tar.gz", []byte("proven tarball bytes"))
				f.write(t, "config-versions/cv-1.tar.gz", []byte("truncated"))

				stats := f.env.SyncArchive(t.Context())
				require.Equal(t, 1, stats.Failed)
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newSyncFixture(t)
			tc.run(t, f)

			tally := f.env.RemoteTally()
			assert.Equal(t, 1, tally.Failed)
			assert.Zero(t, tally.Evicted)
			assert.Zero(t, tally.UploadedBytes)
		})
	}
}

func TestRemoteTallyCanceledContextSettlesNothing(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.write(t, "org.json", []byte(`{"org":"acme"}`))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	f.env.SyncArchive(ctx)

	assert.Zero(t, f.env.RemoteTally(),
		"a cancellation is the wind-down: no motion settles into the run tally")
}

func TestEagerSyncThenSweepSkipsWithoutSecondUpload(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)

	require.NoError(t, f.env.Bytes(t.Context(), "org.json",
		func(context.Context) ([]byte, error) { return []byte(`{"org":"acme"}`), nil }))
	require.Equal(t, 1, f.fake.PutCalls())

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Uploaded, "the sweep sees the eager copy as settled")
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 1, f.fake.PutCalls())
	assert.Zero(t, f.fake.HeadCalls(),
		"the eager Put records a digest, so the gate settles from the inventory alone")
}

func TestSyncArchiveDoesNotEvictATarballWhoseRecordIsNotDurable(t *testing.T) {
	t.Parallel()

	const tarball = "config-versions/cv-1.tar.gz"

	f := newSyncFixture(t)

	// The done record exists only in memory, the state after a failed final
	// flush: the record is the tarball's only local proof once the file is
	// gone, so the eviction must wait. The skip is not a counted failure,
	// because the failed flush already marks the run incomplete.
	data := []byte("tarball")
	f.write(t, tarball, data)
	f.ledger.RecordDone(tarball, manifest.SignatureOf(data))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Evicted)
	assert.Zero(t, stats.Failed)
	assert.True(t, f.exists(t, tarball), "the only local copy stays until its record is durable")

	// Once a flush lands the record, the next sweep evicts normally.
	require.NoError(t, f.ledger.Flush())

	stats = f.env.SyncArchive(t.Context())

	assert.Equal(t, 1, stats.Evicted)
	assert.False(t, f.exists(t, tarball))

	_, ok := f.fake.Object(f.key(tarball))
	assert.True(t, ok)
}

func TestSyncArchiveHealsAcrossStackedRenames(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.resume()

	// Two stacked renames (a project rename above a workspace rename) minted
	// the historical key under BOTH old names, which no single alias
	// substitution spells. The rewrites must compose, or the intact only-copy
	// reads as a permanent hole.
	const (
		sidecar = "projects/newp/workspaces/newws/bundles/logs.gen0001.zip.sidecar.ndjson"
		curZip  = "projects/newp/workspaces/newws/bundles/logs.gen0001.zip"
		oldZip  = "projects/oldp/workspaces/oldws/bundles/logs.gen0001.zip"
	)

	f.write(t, sidecar, []byte(`{"name":"member.json"}`))
	f.fake.SetObject(f.key(oldZip), remotetest.Object{
		Data: []byte("zipbytes"),
		MD5:  remotetest.MD5Sum([]byte("zipbytes")),
	})

	projDir := filepath.Join(f.store.Root(), "projects")
	require.NoError(t, os.Symlink(filepath.Join(projDir, "newp"), filepath.Join(projDir, "oldp")))
	require.NoError(t, os.Symlink(
		filepath.Join(projDir, "newp", "workspaces", "newws"),
		filepath.Join(projDir, "newp", "workspaces", "oldws"),
	))

	stats := f.env.SyncArchive(t.Context())

	require.Zero(t, stats.Failed, "the composed rewrite finds the doubly renamed key")

	healed, ok := f.fake.Object(f.key(curZip))
	require.True(t, ok, "the only-copy converges onto the current key")
	assert.Equal(t, []byte("zipbytes"), healed.Data)
	assert.Contains(t, f.fake.Deleted(), f.key(oldZip))
}

func TestSyncArchiveReleasesAHistoricalCopyLeftByAFailedDelete(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.resume()

	// A heal whose delete failed leaves copies at both keys. The next sweep
	// hits the current key directly, so the release must run from that path
	// too, or the historical duplicate is stranded forever behind the
	// prune's eviction-shape exemption.
	const (
		sidecar = "projects/prod/workspaces/api/bundles/logs.gen0001.zip.sidecar.ndjson"
		curZip  = "projects/prod/workspaces/api/bundles/logs.gen0001.zip"
		oldZip  = "projects/prod/workspaces/zeta-old/bundles/logs.gen0001.zip"
	)

	f.write(t, sidecar, []byte(`{"name":"member.json"}`))
	f.fake.SetObject(f.key(oldZip), remotetest.Object{
		Data: []byte("zipbytes"),
		MD5:  remotetest.MD5Sum([]byte("zipbytes")),
	})

	f.fake.DeleteErr = errors.New("delete boom")
	f.fake.DeleteErrKeys = []string{f.key(oldZip)}

	wsDir := filepath.Join(f.store.Root(), "projects", "prod", "workspaces")
	require.NoError(t, os.Symlink(
		filepath.Join(wsDir, "api"),
		filepath.Join(wsDir, "zeta-old"),
	))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Failed, "a failed release never fails the run")

	_, ok := f.fake.Object(f.key(curZip))
	require.True(t, ok, "the copy landed despite the failed delete")

	_, ok = f.fake.Object(f.key(oldZip))
	require.True(t, ok, "the historical copy survives its failed delete")

	// The fault clears; the next sweep's direct hit releases the leftover.
	f.fake.DeleteErr = nil

	stats = f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Failed)

	_, ok = f.fake.Object(f.key(oldZip))
	assert.False(t, ok, "the direct hit retries the release")

	_, ok = f.fake.Object(f.key(curZip))
	assert.True(t, ok)
}
