package view_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/remote/remotetest"
	"go.jacobcolvin.com/hcp_archiver/view"
)

// Members of the two evicted fixture bundles: plan.log lives in the logs
// bundle, the state blob in the state bundle.
const (
	logsMember  = wsDir + "/runs/run-new/plan.log"
	stateMember = wsDir + "/state-versions/20240101T000000Z-sv-1.tfstate.json"
)

const (
	viewURL    = "s3://view-bucket?region=us-east-1"
	viewPrefix = "hcp"
)

// viewMarker is the org-root marker fixture pointing at [viewURL].
const viewMarker = `{"version":1,"url":"` + viewURL + `","prefix":"` + viewPrefix + `"}`

// evictBundles moves every zip under the fixture workspace's bundles
// directory into fake at its archive-relative key and removes the local
// copy, modeling an archive whose sealed bundles were offloaded. The
// sidecars stay, as eviction leaves them.
func evictBundles(t *testing.T, orgRoot string, fake *remotetest.Fake) {
	t.Helper()

	bundles := filepath.Join(orgRoot, filepath.FromSlash(wsDir), "bundles")

	entries, err := os.ReadDir(bundles)
	require.NoError(t, err)

	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".zip" {
			continue
		}

		abs := filepath.Join(bundles, e.Name())

		data, readErr := os.ReadFile(abs)
		require.NoError(t, readErr)

		key := viewPrefix + "/my-org/" + wsDir + "/bundles/" + e.Name()
		fake.SetObject(key, remotetest.Object{Data: data})
		require.NoError(t, os.Remove(abs))
	}
}

// buildRemoteArchive lays out the standard fixture, evicts its bundles into
// a fake store, and writes the org-root marker.
func buildRemoteArchive(t *testing.T) (string, *remotetest.Fake) {
	t.Helper()

	root := buildArchive(t)
	orgRoot := filepath.Join(root, "my-org")
	fake := remotetest.New()

	evictBundles(t, orgRoot, fake)
	writeFile(t, orgRoot, ".remote.json", viewMarker)

	return root, fake
}

// openRemoteOrg opens the archive at root with a fake-backed client factory,
// the shape every read of an evicted object needs.
func openRemoteOrg(t *testing.T, root string, fake *remotetest.Fake) *view.Org {
	t.Helper()

	return openOrg(t, root,
		view.WithContext(t.Context()),
		view.WithRemoteFactory(func(ctx context.Context, cfg remote.Config) (*remote.Client, error) {
			assert.Equal(t, viewURL, cfg.URL, "the marker's URL drives the client")

			return remote.New(ctx, cfg, remote.WithBucket(fake.Bucket()), remote.WithRetry(0, 0))
		}),
	)
}

// openRemoteWorkspace opens the evicted fixture with a fake-backed client
// factory and returns its workspace plus the fake.
func openRemoteWorkspace(t *testing.T) (*view.Workspace, *remotetest.Fake) {
	t.Helper()

	root, fake := buildRemoteArchive(t)

	return openRemoteOrg(t, root, fake).Workspace("default", "app"), fake
}

func TestWorkspaceOpen_RemoteBundleMember(t *testing.T) {
	t.Parallel()

	ws, fake := openRemoteWorkspace(t)

	// The deflated log member and the stored state blob both read back
	// through ranged reads of the remote zips.
	data, err := ws.Open(wsDir + "/runs/run-new/plan.log")
	require.NoError(t, err)
	assert.Equal(t, "plan output line\n", string(data))

	data, err = ws.Open(wsDir + "/state-versions/20240101T000000Z-sv-1.tfstate.json")
	require.NoError(t, err)
	assert.Equal(t, `{"serial":2}`, string(data))

	ranges := fake.Ranges()
	require.NotEmpty(t, ranges)

	for _, r := range ranges {
		assert.GreaterOrEqual(t, r.Length, int64(0),
			"every remote read must be a bounded ranged request, never a full download")
	}
}

func TestWorkspaceOpen_RemoteCentralDirectoryIsCached(t *testing.T) {
	t.Parallel()

	ws, fake := openRemoteWorkspace(t)

	_, err := ws.Open(wsDir + "/runs/run-new/plan.log")
	require.NoError(t, err)

	heads := fake.HeadCalls()

	_, err = ws.Open(wsDir + "/runs/run-new/plan.log")
	require.NoError(t, err)
	assert.Equal(t, heads, fake.HeadCalls(), "a cached bundle is not re-probed")
}

func TestWorkspaceOpen_RemoteCachedBundleReadsDuringOtherBuild(t *testing.T) {
	t.Parallel()

	ws, fake := openRemoteWorkspace(t)

	// Cache the state bundle first, while no Head is blocked.
	_, err := ws.Open(stateMember)
	require.NoError(t, err)

	// Block the next Head (the logs bundle's build) until released, signaling
	// once it is in flight.
	entered := make(chan struct{})
	release := make(chan struct{})
	fake.HeadHook = func(context.Context) {
		close(entered)
		<-release
	}

	logsDone := make(chan error, 1)
	go func() {
		_, e := ws.Open(logsMember)
		logsDone <- e
	}()

	<-entered // the logs build is parked in Head, mid-flight

	// A read of the already-cached state bundle must not wait on the logs build.
	stateDone := make(chan error, 1)
	go func() {
		_, e := ws.Open(stateMember)
		stateDone <- e
	}()

	select {
	case e := <-stateDone:
		require.NoError(t, e, "a cached bundle reads while an unrelated bundle builds")
	case <-time.After(10 * time.Second):
		t.Fatal("a cached-bundle read blocked on an unrelated in-flight build")
	}

	close(release)
	require.NoError(t, <-logsDone)
}

func TestWorkspaceOpen_RemoteConcurrentSameBundleBuildsOnce(t *testing.T) {
	t.Parallel()

	ws, fake := openRemoteWorkspace(t)

	// Block the first Head until both readers are outstanding, so the two reads
	// of the same bundle are genuinely concurrent.
	entered := make(chan struct{})
	release := make(chan struct{})

	var once sync.Once

	fake.HeadHook = func(context.Context) {
		once.Do(func() { close(entered) })
		<-release
	}

	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, e := ws.Open(logsMember)
			done <- e
		}()
	}

	<-entered      // the single build's Head is in flight
	close(release) // let it and the waiter complete

	require.NoError(t, <-done)
	require.NoError(t, <-done)

	assert.Equal(t, 1, fake.HeadCalls(), "concurrent reads of one bundle build it exactly once")
}

func TestOpenArchive_RemoteMarkerFromNewerBuildIsRejected(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	writeFile(t, filepath.Join(root, "my-org"), ".remote.json",
		`{"version":99,"url":"`+viewURL+`"}`)

	_, err := view.OpenArchive(root, view.WithContext(t.Context()))
	require.ErrorContains(t, err, "newer than this build reads",
		"a marker schema from the future must refuse loudly, not misread")
}

func TestOpenArchive_EmptyRemoteMarkerIsRejectedByName(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	writeFile(t, filepath.Join(root, "my-org"), ".remote.json", `{}`)

	_, err := view.OpenArchive(root, view.WithContext(t.Context()))
	require.ErrorContains(t, err, ".remote.json",
		"a structurally-empty marker must name the file to fix, not defer to a bare"+
			" client error at the first evicted read years later")
	require.ErrorContains(t, err, "no bucket url")
}

func TestWorkspaceOpen_RemoteClientFailureIsRemembered(t *testing.T) {
	t.Parallel()

	root, _ := buildRemoteArchive(t)

	builds := 0

	orgs, err := view.OpenArchive(root,
		view.WithContext(t.Context()),
		view.WithRemoteFactory(func(context.Context, remote.Config) (*remote.Client, error) {
			builds++

			return nil, errors.New("no credential chain")
		}),
	)
	require.NoError(t, err)

	ws := orgs[0].Workspace("default", "app")

	// Every read surfaces the original cause, not a placeholder pointing at a
	// message that scrolled away with the first keypress.
	_, err = ws.Open(wsDir + "/runs/run-new/plan.log")
	require.ErrorContains(t, err, "no credential chain")

	_, err = ws.Open(wsDir + "/runs/run-new/plan.log")
	require.ErrorContains(t, err, "no credential chain")

	assert.Equal(t, 1, builds, "the client build is attempted once per session")
}

func TestWorkspaceOpen_LocalBundleNeverTouchesRemote(t *testing.T) {
	t.Parallel()

	// A marker is present but the zips are still local (eviction not yet
	// run): the local copies stay canonical and no client is ever built.
	root := buildArchive(t)
	writeFile(t, filepath.Join(root, "my-org"), ".remote.json", viewMarker)

	orgs, err := view.OpenArchive(root,
		view.WithContext(t.Context()),
		view.WithRemoteFactory(func(context.Context, remote.Config) (*remote.Client, error) {
			t.Fatal("a local read must not construct a remote client")

			return nil, errors.New("unreachable")
		}),
	)
	require.NoError(t, err)

	data, err := orgs[0].Workspace("default", "app").Open(wsDir + "/runs/run-new/plan.log")
	require.NoError(t, err)
	assert.Equal(t, "plan output line\n", string(data))
}

func TestWorkspaceOpen_LocalOnlyArchiveWithMissingBundle(t *testing.T) {
	t.Parallel()

	// No marker: an evicted-looking gap (sidecar without its zip) is a plain
	// not-found, with no client construction attempted.
	root := buildArchive(t)
	orgRoot := filepath.Join(root, "my-org")
	require.NoError(t, os.Remove(
		filepath.Join(orgRoot, filepath.FromSlash(wsDir), "bundles", "logs.gen0001.zip")))

	orgs, err := view.OpenArchive(root,
		view.WithRemoteFactory(func(context.Context, remote.Config) (*remote.Client, error) {
			t.Fatal("a local-only archive must not construct a remote client")

			return nil, errors.New("unreachable")
		}),
	)
	require.NoError(t, err)

	_, err = orgs[0].Workspace("default", "app").Open(wsDir + "/runs/run-new/plan.log")
	require.ErrorIs(t, err, view.ErrObjectNotFound)
}
