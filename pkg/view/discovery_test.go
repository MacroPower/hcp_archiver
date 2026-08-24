package view_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote/remotetest"
	"go.jacobcolvin.com/hcp_archiver/pkg/view"
)

// entryNames projects a directory listing onto its leaf names.
func entryNames(entries []view.TreeEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}

	return names
}

// openSupplied opens the archive at root against the fake's mirror through a
// supplied remote, the shape a configuration file's remote block produces.
func openSupplied(t *testing.T, root string, fake *remotetest.Fake) []*view.Org {
	t.Helper()

	orgs, err := view.OpenArchive(root,
		view.WithContext(t.Context()),
		view.WithRemote(viewRemoteConfig()),
		view.WithRemoteFactory(fakeFactory(fake)),
	)
	require.NoError(t, err)

	return orgs
}

func TestOpenArchive_CompleteMarkerUnderSuppliedRemoteNeverLists(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	fake := remotetest.New()
	mirrorArchive(t, root, fake)
	writeFile(t, filepath.Join(root, "my-org"), ".remote.json", viewMarker)

	// A key the mirror holds and the local tree does not. A complete marker
	// asserts the sweep proved no such key exists, so a reader that surfaces
	// it is one that enumerated the mirror it was entitled to trust.
	fake.SetObject(viewPrefix+"/my-org/ghost.json", remotetest.Object{Data: []byte("{}")})

	orgs := openSupplied(t, root, fake)
	require.Len(t, orgs, 1)

	assert.Equal(t, 1, fake.ListCalls(),
		"discovery costs one delimited listing, never an inventory of the mirror")

	listed := fake.ListCalls()

	data, err := orgs[0].Read("org.json")
	require.NoError(t, err)
	assert.NotEmpty(t, data)

	entries, err := orgs[0].Entries("")
	require.NoError(t, err)
	assert.NotEmpty(t, entries)
	assert.NotContains(t, entryNames(entries), "ghost.json",
		"a complete organization reads its local tree alone")

	keys, err := orgs[0].List("")
	require.NoError(t, err)
	assert.NotContains(t, keys, "ghost.json")

	_, err = orgs[0].Projects()
	require.NoError(t, err)

	assert.Equal(t, listed, fake.ListCalls(),
		"a complete organization's reads stay local, so none of them lists the mirror")
	assert.Empty(t, fake.Ranges(), "nothing local was fetched through the mirror")
}

func TestOpenArchive_SingleOrgRootNeverListsTheBucket(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	fake := remotetest.New()
	mirrorArchive(t, root, fake)
	writeFile(t, filepath.Join(root, "my-org"), ".remote.json", viewMarker)

	// A second organization the mirror holds but this open must never consider.
	fake.SetObject(viewPrefix+"/other-org/org.json", remotetest.Object{Data: []byte("{}")})

	orgs, err := view.OpenArchive(filepath.Join(root, "my-org"),
		view.WithContext(t.Context()),
		view.WithRemote(viewRemoteConfig()),
		view.WithRemoteFactory(fakeFactory(fake)),
	)
	require.NoError(t, err)

	require.Len(t, orgs, 1)
	assert.Equal(t, "my-org", orgs[0].Name)
	assert.Equal(t, 0, fake.ListCalls(),
		"an organization root resolves through one probe, with no listing at all")
}

func TestOpenArchive_MirrorOrgConfirmedByOrgJSON(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		keys []string
		want []string
	}{
		"an org.json confirms the organization": {
			keys: []string{viewPrefix + "/other-org/org.json"},
			want: []string{"my-org", "other-org"},
		},
		"a prefix holding no org.json contributes none": {
			keys: []string{viewPrefix + "/stray/notes.txt"},
			want: []string{"my-org"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := buildArchive(t)
			fake := remotetest.New()
			mirrorArchive(t, root, fake)

			for _, key := range tc.keys {
				fake.SetObject(key, remotetest.Object{Data: []byte("{}")})
			}

			orgs := openSupplied(t, root, fake)

			got := make([]string, 0, len(orgs))
			for _, org := range orgs {
				got = append(got, org.Name)
			}

			assert.Equal(t, tc.want, got)
		})
	}
}

func TestOpenArchive_MirrorOrgHeadFaultDegradesOnlyThatOrg(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	fake := remotetest.New()
	mirrorArchive(t, root, fake)
	fake.SetObject(viewPrefix+"/other-org/org.json", remotetest.Object{Data: []byte("{}")})

	// The fake heads keys in sorted order, so the first probe is my-org's and
	// the fault lands on it, leaving other-org to answer for itself.
	fake.HeadErr = errors.New("injected probe failure")
	fake.HeadErrN = 1

	orgs := openSupplied(t, root, fake)

	got := make([]string, 0, len(orgs))
	for _, org := range orgs {
		got = append(got, org.Name)
	}

	assert.Equal(t, []string{"my-org", "other-org"}, got,
		"one organization's unreadable org.json must not blank the mirror for the rest")
}

func TestOpenArchive_SuppliedRemoteDegradesWhenDiscoveryFails(t *testing.T) {
	t.Parallel()

	t.Run("a failing listing keeps local content readable", func(t *testing.T) {
		t.Parallel()

		root := buildArchive(t)
		fake := remotetest.New()
		mirrorArchive(t, root, fake)

		fake.ListErr = errors.New("injected listing failure")

		orgs := openSupplied(t, root, fake)
		require.Len(t, orgs, 1)

		entries, err := orgs[0].Entries("")
		require.NoError(t, err)
		assert.NotEmpty(t, entries, "the local tree still browses")

		assert.True(t, orgs[0].HasRemote(), "the remote stays attached so later reads retry")

		// The organization carries no marker, so it stays merged and its own
		// lazy listing runs, fails, and surfaces the warning.
		require.Error(t, orgs[0].RemoteWarning())
	})

	t.Run("a failing client build keeps local content readable", func(t *testing.T) {
		t.Parallel()

		root := buildArchive(t)

		orgs, err := view.OpenArchive(root,
			view.WithContext(t.Context()),
			view.WithRemote(viewRemoteConfig()),
			view.WithRemoteFactory(func(context.Context, remote.Config) (*remote.Client, error) {
				return nil, errors.New("injected client build failure")
			}),
		)
		require.NoError(t, err)
		require.Len(t, orgs, 1)

		entries, entriesErr := orgs[0].Entries("")
		require.NoError(t, entriesErr)
		assert.NotEmpty(t, entries)
	})

	t.Run("a complete organization reports nothing, having needed nothing", func(t *testing.T) {
		t.Parallel()

		root := buildArchive(t)
		fake := remotetest.New()
		mirrorArchive(t, root, fake)
		writeFile(t, filepath.Join(root, "my-org"), ".remote.json", viewMarker)

		fake.ListErr = errors.New("injected listing failure")

		orgs := openSupplied(t, root, fake)
		require.Len(t, orgs, 1)

		entries, err := orgs[0].Entries("")
		require.NoError(t, err)
		assert.NotEmpty(t, entries)

		assert.NoError(t, orgs[0].RemoteWarning(),
			"a complete tree never asked the mirror for a listing, so nothing degraded")
	})
}

func TestOpenArchive_PartialMarkerListsLazilyPerOrg(t *testing.T) {
	t.Parallel()

	fake := buildMirroredArchive(t)

	orgs, dir := openBootstrap(t, fake)
	require.Len(t, orgs, 1)

	marker, err := os.ReadFile(filepath.Join(dir, "my-org", remote.MarkerName))
	require.NoError(t, err)
	assert.Contains(t, string(marker), `"partial": true`)

	opened := fake.ListCalls()
	assert.Equal(t, 1, opened, "the open lists organizations, not their contents")

	_, err = orgs[0].Entries("")
	require.NoError(t, err)

	assert.Greater(t, fake.ListCalls(), opened,
		"a partial organization lists its own prefix, and only once a read needs it")
}
