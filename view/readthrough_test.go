package view_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/remote/remotetest"
	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/view"
)

// mirrorArchive uploads every file under the fixture organization's local
// tree to the fake at its mirrored key, with the digest metadata the
// archiver's uploads record, modeling the close sweep's full mirror. Eviction
// stubs and the remote marker are skipped, as the sweep's eligibility rules
// skip them.
func mirrorArchive(t *testing.T, root string, fake *remotetest.Fake) {
	t.Helper()

	org := filepath.Join(root, "my-org")

	err := filepath.WalkDir(org, func(p string, d os.DirEntry, walkErr error) error {
		require.NoError(t, walkErr)

		if d.IsDir() {
			return nil
		}

		rel, relErr := filepath.Rel(org, p)
		require.NoError(t, relErr)

		slashed := filepath.ToSlash(rel)

		if _, isStub := store.RemoteStubTarget(slashed); isStub || slashed == remote.MarkerName {
			return nil
		}

		data, readErr := os.ReadFile(p)
		require.NoError(t, readErr)

		sum := sha256.Sum256(data)
		fake.SetObject(viewPrefix+"/my-org/"+slashed, remotetest.Object{
			Data:     data,
			Metadata: map[string]string{"sha256": hex.EncodeToString(sum[:])},
		})

		return nil
	})
	require.NoError(t, err)
}

// buildMirroredArchive builds the standard fixture and mirrors it whole into
// a fake store. The local tree is discarded conceptually: callers that model
// a machine with no local files open a fresh directory against the fake.
func buildMirroredArchive(t *testing.T) *remotetest.Fake {
	t.Helper()

	root := buildArchive(t)
	fake := remotetest.New()
	mirrorArchive(t, root, fake)

	return fake
}

// fakeFactory returns a client factory backed by the fake, with retries off
// so an injected fault surfaces once.
func fakeFactory(fake *remotetest.Fake) func(context.Context, remote.Config) (*remote.Client, error) {
	return func(ctx context.Context, cfg remote.Config) (*remote.Client, error) {
		//nolint:wrapcheck // A transparent test factory.
		return remote.New(ctx, cfg, remote.WithBucket(fake.Bucket()), remote.WithRetry(0, 0))
	}
}

// viewRemoteConfig is the supplied-remote counterpart of the marker fixture.
func viewRemoteConfig() remote.Config {
	return remote.Config{URL: viewURL, Prefix: viewPrefix}
}

// openBootstrap opens an empty directory against the fake's mirror, the
// completely-absent-local-files shape, returning the organizations and the
// directory they materialize into.
func openBootstrap(t *testing.T, fake *remotetest.Fake) ([]*view.Org, string) {
	t.Helper()

	dir := t.TempDir()

	orgs, err := view.OpenArchive(dir,
		view.WithContext(t.Context()),
		view.WithRemote(viewRemoteConfig()),
		view.WithRemoteFactory(fakeFactory(fake)),
	)
	require.NoError(t, err)

	return orgs, dir
}

func TestOpenArchive_BootstrapFromEmptyDir(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)

	orgs, dir := openBootstrap(t, fake)
	require.Len(t, orgs, 1)
	assert.Equal(t, "my-org", orgs[0].Name)

	// The org root is materialized as self-describing: org.json fetched, the
	// marker recording both the mirror and the tree's partial nature.
	assert.FileExists(t, filepath.Join(dir, "my-org", "org.json"))

	marker, err := os.ReadFile(filepath.Join(dir, "my-org", remote.MarkerName))
	require.NoError(t, err)
	assert.Contains(t, string(marker), viewURL)
	assert.Contains(t, string(marker), `"partial": true`)
}

func TestOpenArchive_EstablishedOrgStaysUnmarked(t *testing.T) {
	t.Parallel()

	// A local organization that already carries its org.json but no marker is
	// not a browse cache: a read-only open with a supplied remote must not
	// stamp it partial and flip every later flag-less open into merged mode.
	fake := buildMirroredArchive(t)
	root := buildArchive(t)

	orgs, err := view.OpenArchive(root,
		view.WithContext(t.Context()),
		view.WithRemote(viewRemoteConfig()),
		view.WithRemoteFactory(fakeFactory(fake)),
	)
	require.NoError(t, err)
	require.NotEmpty(t, orgs)

	assert.NoFileExists(t, filepath.Join(root, "my-org", remote.MarkerName),
		"a read-only open leaves an established organization's root unmutated")
}

func TestOpenArchive_BootstrapFromMissingDir(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)

	// The most natural first invocation of the restore flow names a directory
	// that does not exist yet; it bootstraps exactly like an empty one.
	dir := filepath.Join(t.TempDir(), "restore", "archive")

	orgs, err := view.OpenArchive(dir,
		view.WithContext(t.Context()),
		view.WithRemote(viewRemoteConfig()),
		view.WithRemoteFactory(fakeFactory(fake)),
	)
	require.NoError(t, err)
	require.Len(t, orgs, 1)
	assert.Equal(t, "my-org", orgs[0].Name)

	assert.FileExists(t, filepath.Join(dir, "my-org", "org.json"),
		"the bootstrap creates the directory on the first fetch")
}

func TestOpenArchive_BootstrappedMarkerCarriesTheRemote(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)
	_, dir := openBootstrap(t, fake)

	// A later session passes no remote at all: the written marker alone must
	// reconnect the mirror and keep reads falling through.
	orgs, err := view.OpenArchive(dir,
		view.WithContext(t.Context()),
		view.WithRemoteFactory(fakeFactory(fake)),
	)
	require.NoError(t, err)
	require.Len(t, orgs, 1)

	data, err := orgs[0].ReadFile(wsDir + "/workspace.json")
	require.NoError(t, err)
	assert.Contains(t, string(data), `"name":"app"`)
	assert.FileExists(t, filepath.Join(dir, "my-org", filepath.FromSlash(wsDir), "workspace.json"),
		"a fetched object persists at its archive path")
}

func TestOpenArchive_PartialLocalMergeAddsMirrorOrgs(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	fake := remotetest.New()
	mirrorArchive(t, root, fake)

	fake.SetObject(viewPrefix+"/other-org/org.json", remotetest.Object{
		Data: []byte(`{"data":{"id":"org-2","type":"organizations"}}`),
	})

	orgs, err := view.OpenArchive(root,
		view.WithContext(t.Context()),
		view.WithRemote(viewRemoteConfig()),
		view.WithRemoteFactory(fakeFactory(fake)),
	)
	require.NoError(t, err)
	require.Len(t, orgs, 2)
	assert.Equal(t, "my-org", orgs[0].Name)
	assert.Equal(t, "other-org", orgs[1].Name)

	assert.FileExists(t, filepath.Join(root, "other-org", "org.json"),
		"a mirror-only organization is materialized")
}

func TestOpenArchive_MirrorOrgNameConfinedToTheArchive(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		// The org segment is what a crafted listing key carries where the
		// organization name belongs, in "<prefix>/<org>/org.json".
		org string
		// A true want records that the segment names a real organization; the
		// rest are refused, since joining them onto the archive root reaches
		// the root itself or somewhere above it.
		want bool
	}{
		"parent":          {org: ".."},
		"self":            {org: "."},
		"parent chain":    {org: "../.."},
		"windows parent":  {org: `..\..`},
		"windows nested":  {org: `sub\..\..`},
		"control byte":    {org: "other\norg"},
		"plain name":      {org: "other-org", want: true},
		"dotted name":     {org: "..leading", want: true},
		"name with a dot": {org: "acme.corp", want: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake := buildMirroredArchive(t)
			cfg := viewRemoteConfig()
			body := remotetest.Object{Data: []byte(`{"data":{"id":"org-2","type":"organizations"}}`)}

			// Two objects, the pair a mirror-controlling attacker plants: the
			// listing key whose first segment becomes the organization name,
			// and the object the materialization's own fetch key resolves to
			// once path.Join cleans it, so an unconfined open completes its
			// write rather than merely attempting it.
			fake.SetObject(viewPrefix+"/"+tc.org+"/org.json", body)
			fake.SetObject(cfg.Key(tc.org, "org.json"), body)

			// A nested directory, so the parent an escaping join lands in is
			// the test's own and observably empty.
			dir := filepath.Join(t.TempDir(), "archive")

			orgs, err := view.OpenArchive(dir,
				view.WithContext(t.Context()),
				view.WithRemote(cfg),
				view.WithRemoteFactory(fakeFactory(fake)),
			)
			require.NoError(t, err)

			var names []string

			for _, org := range orgs {
				names = append(names, org.Name)
			}

			if tc.want {
				assert.Contains(t, names, tc.org)
				assert.FileExists(t, filepath.Join(dir, tc.org, "org.json"),
					"a well-named mirror organization still materializes")

				return
			}

			assert.Equal(t, []string{"my-org"}, names,
				"the crafted segment names no organization")

			for _, leaf := range []string{"org.json", remote.MarkerName} {
				assert.NoFileExists(t, filepath.Join(dir, leaf),
					"materialization never writes over the archive root itself")
				assert.NoFileExists(t, filepath.Join(filepath.Dir(dir), leaf),
					"materialization never writes outside the archive root")
			}
		})
	}
}

func TestOpenArchive_RemoteMismatchRefused(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	writeFile(t, filepath.Join(root, "my-org"), remote.MarkerName, viewMarker)

	fake := remotetest.New()

	_, err := view.OpenArchive(root,
		view.WithContext(t.Context()),
		view.WithRemote(remote.Config{URL: "s3://elsewhere", Prefix: viewPrefix}),
		view.WithRemoteFactory(fakeFactory(fake)),
	)
	require.ErrorIs(t, err, view.ErrRemoteMismatch)
}

func TestReadFile_ReadThroughPersistsAndStaysLocal(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)
	orgs, dir := openBootstrap(t, fake)

	data, err := orgs[0].ReadFile(wsDir + "/variables.json")
	require.NoError(t, err)
	assert.Contains(t, string(data), "region")
	assert.FileExists(t, filepath.Join(dir, "my-org", filepath.FromSlash(wsDir), "variables.json"))

	heads := fake.HeadCalls()

	again, err := orgs[0].ReadFile(wsDir + "/variables.json")
	require.NoError(t, err)
	assert.Equal(t, data, again)
	assert.Equal(t, heads, fake.HeadCalls(), "the second read is served from disk")
}

func TestReadFile_TraversalRefused(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)

	// Seed a key in a sibling organization's namespace: a ".." path that
	// slipped through would collapse into it via the mirror key's path.Join.
	fake.SetObject(viewPrefix+"/other-org/secret.json", remotetest.Object{
		Data: []byte(`{"leak":true}`),
	})

	orgs, dir := openBootstrap(t, fake)

	_, err := orgs[0].ReadFile("../other-org/secret.json")
	require.ErrorIs(t, err, view.ErrInvalidPath,
		"a path escaping the organization is refused, not cleaned into a sibling's key")

	assert.NoFileExists(t, filepath.Join(dir, "my-org", "other-org", "secret.json"),
		"nothing is fetched or persisted for a refused path")
}

func TestRead_ReadThroughPersists(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)

	content := []byte(`{"data":{"id":"user-9","type":"users"}}`)
	sum := sha256.Sum256(content)
	fake.SetObject(viewPrefix+"/my-org/users/user-9.json", remotetest.Object{
		Data:     content,
		Metadata: map[string]string{"sha256": hex.EncodeToString(sum[:])},
	})

	orgs, dir := openBootstrap(t, fake)

	// Org.Read is the show command's path; its loose branch must fetch and
	// persist exactly as ReadFile does.
	data, err := orgs[0].Read("users/user-9.json")
	require.NoError(t, err)
	assert.Equal(t, content, data)
	assert.FileExists(t, filepath.Join(dir, "my-org", "users", "user-9.json"))
}

func TestReadFile_DigestMismatchLeavesNoFile(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)

	sum := sha256.Sum256([]byte("what the digest promises"))
	fake.SetObject(viewPrefix+"/my-org/users/rotten.json", remotetest.Object{
		Data:     []byte("what the mirror actually serves"),
		Metadata: map[string]string{"sha256": hex.EncodeToString(sum[:])},
	})

	orgs, dir := openBootstrap(t, fake)

	_, err := orgs[0].ReadFile("users/rotten.json")
	require.ErrorContains(t, err, "digest")

	usersDir := filepath.Join(dir, "my-org", "users")
	assert.NoFileExists(t, filepath.Join(usersDir, "rotten.json"))

	entries, readErr := os.ReadDir(usersDir)
	if readErr == nil {
		for _, e := range entries {
			assert.False(t, strings.HasPrefix(e.Name(), ".atomicfile-"),
				"a refused fetch must leave no staging file behind")
		}
	}
}

func TestWorkspace_SealedFormsFromEmptyDir(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)
	orgs, dir := openBootstrap(t, fake)

	ws := orgs[0].Workspace("default", "app")

	// A rolled-up member: the roll-up is materialized locally, then served
	// out of it.
	data, err := ws.Open(wsDir + "/runs/run-new/config-version.json")
	require.NoError(t, err)
	assert.Contains(t, string(data), "cv-1")
	assert.FileExists(t,
		filepath.Join(dir, "my-org", filepath.FromSlash(wsDir), "rollups", "config-versions.ndjson"))

	// A bundled member: served by ranged reads of the remote zip, which is
	// never pulled down whole.
	data, err = ws.Open(wsDir + "/runs/run-new/plan.log")
	require.NoError(t, err)
	assert.Equal(t, "plan output line\n", string(data))
	assert.NoFileExists(t,
		filepath.Join(dir, "my-org", filepath.FromSlash(wsDir), "bundles", "logs.gen0001.zip"))
	assert.NotEmpty(t, fake.Ranges(), "bundle members read as ranged requests")
}

func TestWorkspace_RunsAndStateVersionsFromEmptyDir(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)
	orgs, _ := openBootstrap(t, fake)

	require.Equal(t, []string{"default"}, mustProjects(t, orgs[0]))

	ws := orgs[0].Workspace("default", "app")

	runs, err := ws.Runs()
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, "run-new", runs[0].ID)
	assert.Equal(t, "applied", runs[0].Status)
	assert.Equal(t, "run-old", runs[1].ID)

	svs, err := ws.StateVersions()
	require.NoError(t, err)
	require.Len(t, svs, 2)
	assert.True(t, svs[0].HasRaw, "a mirror-only loose blob still reports present")
}

// mustProjects returns the organization's project names.
func mustProjects(t *testing.T, org *view.Org) []string {
	t.Helper()

	projects, err := org.Projects()
	require.NoError(t, err)

	return projects
}

func TestExists_MirrorOnlyLooseFileWithoutFetching(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)
	orgs, dir := openBootstrap(t, fake)

	ws := orgs[0].Workspace("default", "app")
	rel := wsDir + "/state-versions/20240102T030405Z-sv-2.tfstate.json"

	ok, err := ws.Exists(rel)
	require.NoError(t, err)
	assert.True(t, ok)

	assert.NoFileExists(t, filepath.Join(dir, "my-org", filepath.FromSlash(rel)),
		"a presence probe answers from the cached listing without fetching")
}

func TestList_MergedEntriesFromEmptyDir(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)
	orgs, _ := openBootstrap(t, fake)

	entries, err := orgs[0].List("")
	require.NoError(t, err)

	byPath := make(map[string]view.Entry, len(entries))
	for _, e := range entries {
		byPath[e.Path] = e
	}

	ws, ok := byPath[wsDir+"/workspace.json"]
	require.True(t, ok, "a mirror-only warm file lists")
	assert.True(t, ws.Offloaded, "its bytes are only in the mirror until read")
	assert.Equal(t, view.FormLoose, ws.Form)
	assert.Positive(t, ws.Size)

	plan, ok := byPath[wsDir+"/runs/run-new/plan.log"]
	require.True(t, ok, "a sealed member lists through its materialized sidecar")
	assert.Equal(t, view.FormBundle, plan.Form)
	assert.True(t, plan.Offloaded, "its bundle zip is not on disk")

	// The merge pass applies its own machinery filter to mirror keys, which
	// the local-only machinery test never exercises.
	for p := range byPath {
		assert.NotContains(t, p, store.RollupsDirName+"/", "mirror machinery keys stay hidden")
		assert.NotContains(t, p, store.BundlesDirName+"/", "mirror machinery keys stay hidden")
		assert.NotEqual(t, remote.MarkerName, p)
	}
}

func TestList_EvictedTarballListsOnce(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	fake := remotetest.New()

	// A local stub and the tarball's own mirror key describe one object; the
	// listing must not emit it from both sides.
	rel := evictTarballRemote(t, root, "cv-once", fake, tarballContent)
	mirrorArchive(t, root, fake)

	orgs, err := view.OpenArchive(root,
		view.WithContext(t.Context()),
		view.WithRemote(viewRemoteConfig()),
		view.WithRemoteFactory(fakeFactory(fake)),
	)
	require.NoError(t, err)

	entries, err := orgs[0].List("")
	require.NoError(t, err)

	count := 0

	for _, e := range entries {
		if e.Path == rel {
			count++

			assert.True(t, e.Offloaded)
		}
	}

	assert.Equal(t, 1, count)
}

func TestRead_EvictedTarballStaysRemoteOnly(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)

	rel := store.ConfigVersionsDirName + "/cv-9.tar.gz"
	sum := sha256.Sum256([]byte(tarballContent))
	fake.SetObject(viewPrefix+"/my-org/"+rel, remotetest.Object{
		Data:     []byte(tarballContent),
		Metadata: map[string]string{"sha256": hex.EncodeToString(sum[:])},
	})

	orgs, dir := openBootstrap(t, fake)

	// Even with no local stub, the mirror's inventory proves the eviction; a
	// whole-object read still refuses to buffer the unbounded surface.
	_, err := orgs[0].Read(rel)
	require.ErrorIs(t, err, view.ErrRemoteOnly)
	require.ErrorContains(t, err, viewPrefix+"/my-org/"+rel, "the mirrored key is named")

	assert.NoFileExists(t, filepath.Join(dir, "my-org", filepath.FromSlash(rel)),
		"read-through never re-materializes an evicted surface")
}

func TestUnseal_FetchesTarballWithoutLocalStub(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)

	rel := store.ConfigVersionsDirName + "/cv-9.tar.gz"
	sum := sha256.Sum256([]byte(tarballContent))
	fake.SetObject(viewPrefix+"/my-org/"+rel, remotetest.Object{
		Data:     []byte(tarballContent),
		Metadata: map[string]string{"sha256": hex.EncodeToString(sum[:])},
	})

	orgs, dir := openBootstrap(t, fake)
	target := t.TempDir()

	sum2, err := view.NewArchive(orgs).Unseal(t.Context(), target, "my-org/"+rel, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, sum2.Files)
	assert.Zero(t, sum2.Errored)

	recovered, err := os.ReadFile(filepath.Join(target, "my-org", filepath.FromSlash(rel)))
	require.NoError(t, err)
	assert.Equal(t, tarballContent, string(recovered))

	assert.NoFileExists(t, filepath.Join(dir, "my-org", filepath.FromSlash(rel)),
		"the tarball streams to the target without landing in the archive")
}

func TestOpenArchive_OfflineMarkerArchiveDegradesToLocal(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)

	// A partial marker (a bootstrapped tree) whose mirror cannot be reached:
	// the local content must still browse, with the failure surfaced once.
	writeFile(t, filepath.Join(root, "my-org"), remote.MarkerName,
		`{"version":1,"url":"`+viewURL+`","prefix":"`+viewPrefix+`","partial":true}`)

	orgs, err := view.OpenArchive(root,
		view.WithContext(t.Context()),
		view.WithRemoteFactory(func(context.Context, remote.Config) (*remote.Client, error) {
			return nil, errors.New("no credential chain")
		}),
	)
	require.NoError(t, err)
	require.Len(t, orgs, 1)

	assert.Equal(t, []string{"default"}, mustProjects(t, orgs[0]))

	entries, err := orgs[0].List("")
	require.NoError(t, err)
	assert.NotEmpty(t, entries, "the local listing survives an unreachable mirror")

	require.Error(t, orgs[0].RemoteWarning(), "the degradation is not silent")
	require.ErrorContains(t, orgs[0].RemoteWarning(), "no credential chain")
}

func TestCompleteMarkerArchiveKeepsLocalReadsLocal(t *testing.T) {
	t.Parallel()

	// An archiver-written marker (no partial flag) means the tree is
	// complete: no listing, no fetch-through, no client, even for a miss.
	root := buildArchive(t)
	writeFile(t, filepath.Join(root, "my-org"), remote.MarkerName, viewMarker)

	orgs, err := view.OpenArchive(root,
		view.WithContext(t.Context()),
		view.WithRemoteFactory(func(context.Context, remote.Config) (*remote.Client, error) {
			t.Error("a complete archive's reads must not construct a remote client")

			return nil, errors.New("unreachable")
		}),
	)
	require.NoError(t, err)

	assert.Equal(t, []string{"default"}, mustProjects(t, orgs[0]))

	_, err = orgs[0].ReadFile("users/absent.json")
	require.ErrorIs(t, err, fs.ErrNotExist, "a miss stays a plain local miss")

	_, err = orgs[0].Read("users/absent.json")
	require.ErrorIs(t, err, view.ErrObjectNotFound)
}

func TestEntries_MergedFilesScreenListing(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)
	orgs, _ := openBootstrap(t, fake)

	entries, err := orgs[0].Entries("")
	require.NoError(t, err)

	names := make(map[string]view.TreeEntry, len(entries))
	for _, e := range entries {
		names[e.Name] = e
	}

	require.Contains(t, names, "projects")
	assert.True(t, names["projects"].Dir)

	// The bootstrap materialized org.json, so it lists as local.
	require.Contains(t, names, "org.json")
	assert.False(t, names["org.json"].Remote)

	wsEntries, err := orgs[0].Entries(wsDir)
	require.NoError(t, err)

	var wsJSON *view.TreeEntry

	for _, e := range wsEntries {
		if e.Name == "workspace.json" {
			wsJSON = &e

			break
		}
	}

	require.NotNil(t, wsJSON)
	assert.True(t, wsJSON.Remote, "a mirror-only file is flagged for the browser row")
	assert.Positive(t, wsJSON.Size)
}
