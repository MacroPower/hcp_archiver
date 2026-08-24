package view_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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
// supplied remote, the shape a configuration file's remote section produces.
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
	// it has enumerated a mirror the marker said it could trust.
	fake.SetObject(viewPrefix+"/my-org/ghost.json", remotetest.Object{Data: []byte("{}")})

	orgs := openSupplied(t, root, fake)
	require.Len(t, orgs, 1)

	assert.Equal(t, 1, fake.ListCalls(),
		"discovery costs one delimited listing, never an inventory of the mirror")

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

	assert.Equal(t, 1, fake.ListCalls(),
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

func TestOpenArchive_MirrorOrgProbeFaultConfinesItselfToThatOrg(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	fake := remotetest.New()
	mirrorArchive(t, root, fake)
	fake.SetObject(viewPrefix+"/other-org/org.json", remotetest.Object{Data: []byte("{}")})
	fake.SetObject(viewPrefix+"/third-org/org.json", remotetest.Object{Data: []byte("{}")})

	// The probes fan out, so the failing one is named rather than ordered.
	fake.HeadErr = errors.New("injected probe failure")
	fake.HeadErrKeys = []string{viewPrefix + "/other-org/" + "org.json"}

	orgs := openSupplied(t, root, fake)

	got := make([]string, 0, len(orgs))
	for _, org := range orgs {
		got = append(got, org.Name)
	}

	assert.Equal(t, []string{"my-org", "third-org"}, got,
		"one organization's unreadable org.json must not blank the mirror for the rest")

	// A local organization whose probe never answered degrades rather than
	// opening as though the mirror had denied it: the remote stays attached
	// and its content still merges.
	local := buildArchive(t)
	writeFile(t, filepath.Join(local, "other-org"), "org.json", `{"data":{"id":"org-2"}}`)

	localFake := remotetest.New()
	mirrorArchive(t, local, localFake)
	localFake.SetObject(viewPrefix+"/other-org/org.json", remotetest.Object{Data: []byte("{}")})
	localFake.SetObject(viewPrefix+"/other-org/only-remote.json", remotetest.Object{Data: []byte("{}")})

	localFake.HeadErr = errors.New("injected probe failure")
	localFake.HeadErrKeys = []string{viewPrefix + "/other-org/" + "org.json"}

	degraded := openSupplied(t, local, localFake)
	require.Len(t, degraded, 2)

	other := degraded[1]
	require.Equal(t, "other-org", other.Name)
	assert.True(t, other.HasRemote(), "an unproven organization keeps its remote")

	entries, err := other.Entries("")
	require.NoError(t, err)
	assert.Contains(t, entryNames(entries), "only-remote.json",
		"an unproven organization still reads through, rather than falling back to local content alone")
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

	t.Run("a bootstrap whose every probe fails reports the fault", func(t *testing.T) {
		t.Parallel()

		fake := buildMirroredArchive(t)

		// A credential that lists but cannot head, the shape a prefix-scoped
		// read-only policy gets wrong. Reporting an empty mirror here would
		// assert the opposite of what the fault said.
		fake.HeadErr = errors.New("injected probe failure")

		_, err := view.OpenArchive(t.TempDir(),
			view.WithContext(t.Context()),
			view.WithRemote(viewRemoteConfig()),
			view.WithRemoteFactory(fakeFactory(fake)),
		)
		require.ErrorIs(t, err, view.ErrNotArchive)
		assert.ErrorContains(t, err, "injected probe failure",
			"a discovery that confirmed nothing names why, rather than reporting an empty mirror")
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

		assert.True(t, orgs[0].HasRemote(), "the remote stays attached so later reads retry")
		require.Error(t, orgs[0].RemoteWarning(),
			"the build that failed is reported once a merged read has asked for it")
	})

	t.Run("a complete organization needs no listing, so it reports nothing", func(t *testing.T) {
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

func TestOpenArchive_SlowListingNamesItsOrganization(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	fake := remotetest.New()
	mirrorArchive(t, root, fake)

	release := make(chan struct{})
	noticed := make(chan string, 1)

	// The org-inventory listing blocks past the grace period, the shape a
	// mirror large enough to page for minutes has. Discovery's own delimited
	// listing runs first and must not block, or the open never returns.
	var listings atomic.Int64

	fake.ListHook = func(context.Context) {
		if listings.Add(1) > 1 {
			<-release
		}
	}

	orgs, err := view.OpenArchive(root,
		view.WithContext(t.Context()),
		view.WithRemote(viewRemoteConfig()),
		view.WithRemoteFactory(fakeFactory(fake)),
		view.WithListNoticeGraceForTest(10*time.Millisecond),
		view.WithListNotice(func(org string) {
			select {
			case noticed <- org:
			default:
			}
		}),
	)
	require.NoError(t, err)
	require.Len(t, orgs, 1)

	done := make(chan struct{})

	go func() {
		defer close(done)

		//nolint:errcheck // The listing is released mid-flight; its result is not the subject.
		orgs[0].Entries("")
	}()

	select {
	case org := <-noticed:
		assert.Equal(t, "my-org", org, "the notice names the organization being listed")
	case <-time.After(10 * time.Second):
		t.Fatal("a listing past its grace period reported nothing")
	}

	close(release)
	<-done
}

func TestOpenArchive_PromptListingReportsNothing(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	fake := remotetest.New()
	mirrorArchive(t, root, fake)

	var notices atomic.Int64

	orgs, err := view.OpenArchive(root,
		view.WithContext(t.Context()),
		view.WithRemote(viewRemoteConfig()),
		view.WithRemoteFactory(fakeFactory(fake)),
		view.WithListNotice(func(string) { notices.Add(1) }),
	)
	require.NoError(t, err)
	require.Len(t, orgs, 1)

	_, err = orgs[0].Entries("")
	require.NoError(t, err)

	assert.Zero(t, notices.Load(), "a listing that settles inside the grace period explains nothing")
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
