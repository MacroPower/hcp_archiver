package view_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/view"
)

// The fixture workspace's sealed members and their carrying containers, as
// buildArchive lays them out.
const (
	planPath   = wsDir + "/runs/run-new/plan.log"
	cvPath     = wsDir + "/runs/run-new/config-version.json"
	svMetaPath = wsDir + "/state-versions/20240101T000000Z-sv-1.meta.json"
	svBlobPath = wsDir + "/state-versions/20240101T000000Z-sv-1.tfstate.json"

	logsBundle    = wsDir + "/bundles/logs.gen0001.zip"
	cvRollup      = wsDir + "/rollups/config-versions.ndjson"
	cvContent     = `{"data":{"id":"cv-1","type":"configuration-versions","attributes":{"source":"tfe-api"}}}`
	planContent   = "plan output line\n"
	svBlobContent = `{"serial":2}`
)

// mapEntries projects a listing onto one string per entry.
func mapEntries(entries []view.Entry, get func(view.Entry) string) []string {
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, get(e))
	}

	return paths
}

// entryPaths projects a listing onto its archive-relative paths.
func entryPaths(entries []view.Entry) []string {
	return mapEntries(entries, func(e view.Entry) string { return e.Path })
}

// entryByPath finds one listing entry by its archive-relative path.
func entryByPath(t *testing.T, entries []view.Entry, rel string) view.Entry {
	t.Helper()

	for _, e := range entries {
		if e.Path == rel {
			return e
		}
	}

	t.Fatalf("no entry for %s", rel)

	return view.Entry{}
}

func TestOrgList(t *testing.T) {
	t.Parallel()

	// The fixture's full logical listing: every archived object exactly once,
	// sorted by path, whichever physical form holds it.
	wholeOrg := []string{
		"org.json",
		"projects/default/project.json",
		wsDir + "/runs/run-new/config-version.json",
		wsDir + "/runs/run-new/plan.log",
		wsDir + "/runs/run-new/run.json",
		wsDir + "/runs/run-old/comments.json",
		wsDir + "/runs/run-old/run.history.ndjson",
		wsDir + "/runs/run-old/run.json",
		wsDir + "/state-versions/20240101T000000Z-sv-1.meta.json",
		wsDir + "/state-versions/20240101T000000Z-sv-1.tfstate.json",
		wsDir + "/state-versions/20240102T030405Z-sv-2.meta.json",
		wsDir + "/state-versions/20240102T030405Z-sv-2.tfstate.json",
		wsDir + "/variables.json",
		wsDir + "/workspace.json",
	}

	tests := map[string]struct {
		prefix string
		want   []string
		err    error
	}{
		"whole org": {
			prefix: "",
			want:   wholeOrg,
		},
		"projects subtree omits the org document": {
			prefix: "projects",
			want:   wholeOrg[1:],
		},
		"single project": {
			prefix: "projects/default",
			want:   wholeOrg[1:],
		},
		"single workspace": {
			prefix: wsDir,
			want:   wholeOrg[2:],
		},
		"run directory spans all three forms": {
			prefix: wsDir + "/runs/run-new",
			want: []string{
				wsDir + "/runs/run-new/config-version.json",
				wsDir + "/runs/run-new/plan.log",
				wsDir + "/runs/run-new/run.json",
			},
		},
		"single sealed file": {
			prefix: planPath,
			want:   []string{planPath},
		},
		"single loose file": {
			prefix: "org.json",
			want:   []string{"org.json"},
		},
		"prefix matches whole segments only": {
			prefix: wsDir + "/runs/run-ne",
			want:   nil,
		},
		"machinery directory lists nothing": {
			prefix: wsDir + "/bundles",
			want:   nil,
		},
		"missing project is empty, not an error": {
			prefix: "projects/nope",
			want:   nil,
		},
		"traversal is refused": {
			prefix: "../etc/passwd",
			err:    view.ErrInvalidPath,
		},
		"absolute path is refused": {
			prefix: "/etc/passwd",
			err:    view.ErrInvalidPath,
		},
		"bare slash is refused": {
			prefix: "/",
			err:    view.ErrInvalidPath,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			org := openOrg(t, buildArchive(t))

			entries, err := org.List(tt.prefix)
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)

				return
			}

			require.NoError(t, err)

			if len(tt.want) == 0 {
				assert.Empty(t, entries)
			} else {
				assert.Equal(t, tt.want, entryPaths(entries))
			}
		})
	}
}

func TestOrgList_MachineryNeverAppears(t *testing.T) {
	t.Parallel()

	// The extended fixture carries every machinery shape: bundles and their
	// sidecars, roll-ups, a ledger shard, a staging leftover, identity
	// sidecars, plus an org-root remote marker and an eviction stub this test
	// adds.
	root := buildUnsealArchive(t)
	writeFile(t, filepath.Join(root, "my-org"), ".remote.json", viewMarker)
	evictTarball(t, root, "cv-9")

	org := openOrg(t, root)

	entries, err := org.List("")
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	for _, p := range entryPaths(entries) {
		assert.NotContains(t, p, "/bundles/", "bundle zips and sidecars are hidden: %s", p)
		assert.NotContains(t, p, "/rollups/", "roll-up files are hidden: %s", p)
		assert.NotContains(t, p, ".sidecar", "sidecar indexes are hidden: %s", p)
		assert.NotContains(t, p, ".ledger", "ledger shards are hidden: %s", p)
		// A substring check, so it covers both the org marker and the
		// per-object eviction stubs that share its spelling.
		assert.NotContains(t, p, ".remote.json", "remote markers and stubs are hidden: %s", p)
		assert.NotContains(t, p, ".identity.json", "identity sidecars are hidden: %s", p)
		assert.NotContains(t, p, ".atomicfile-", "staging leftovers are hidden: %s", p)
	}
}

func TestOrgList_EvictedTarball(t *testing.T) {
	t.Parallel()

	// A configuration-version tarball evicted to the remote store leaves no
	// file at all, only its stub. The object still lists, at its true size and
	// flagged, rather than reading as one the archive never collected.
	root := buildArchive(t)
	tarball := evictTarball(t, root, "cv-9")

	org := openOrg(t, root)

	entries, err := org.List("")
	require.NoError(t, err)

	e := entryByPath(t, entries, tarball)
	assert.Equal(t, view.FormLoose, e.Form, "a tarball is a plain file wherever it lives")
	assert.True(t, e.Offloaded)
	assert.False(t, org.HasRemote(), "the fixture records no mirror, so nothing can fetch it back")
	assert.EqualValues(t, len(tarballContent), e.Size, "the stub's recorded size works offline")
	assert.Empty(t, e.Container)
	assert.True(t, e.ModTime.IsZero(), "an evicted object has no local file to date it")

	assert.NotContains(t, entryPaths(entries), store.RemoteStubPath(tarball),
		"the stub stands in for the object without listing as one")
}

func TestOrgList_EvictedTarballPrefixes(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	tarball := evictTarball(t, root, "cv-9")

	org := openOrg(t, root)

	// Addressing the evicted object directly is the natural thing to type
	// after a read reports it remote-only, and it must find it: the path
	// resolves to nothing on disk, so only the stub beside it answers.
	entries, err := org.List(tarball)
	require.NoError(t, err)
	assert.Equal(t, []string{tarball}, entryPaths(entries))

	// Addressing the stub is addressing machinery. Its object sits outside the
	// requested prefix, so the listing is empty rather than answering with a
	// path the caller did not ask about.
	entries, err = org.List(store.RemoteStubPath(tarball))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestOrgHasRemote(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	assert.False(t, openOrg(t, root).HasRemote(), "an archive with no marker has no mirror")

	mirrored := buildArchive(t)
	writeFile(t, filepath.Join(mirrored, "my-org"), ".remote.json", viewMarker)
	assert.True(t, openOrg(t, mirrored).HasRemote(), "the org-root marker is what records one")
}

func TestOrgList_OffloadedBundleMemberWithoutMirror(t *testing.T) {
	t.Parallel()

	// A bundle whose zip is gone with no marker beside it: its members still
	// list, from the sidecar index eviction left behind, and nothing can fetch
	// them. An unseal counts them as unrecoverable on exactly this pairing,
	// which is why the two facts have to be readable together.
	root := buildArchive(t)
	require.NoError(t, os.Remove(
		filepath.Join(root, "my-org", filepath.FromSlash(wsDir), "bundles", "logs.gen0001.zip")))

	org := openOrg(t, root)

	entries, err := org.List("")
	require.NoError(t, err)

	e := entryByPath(t, entries, wsDir+"/runs/run-new/plan.log")
	assert.Equal(t, view.FormBundle, e.Form)
	assert.True(t, e.Offloaded, "the member's bytes are not here")
	assert.False(t, org.HasRemote(), "and there is nowhere to fetch them from")
}

func TestOrgList_LocalTarballBeatsItsStub(t *testing.T) {
	t.Parallel()

	// A restore brings the tarball back beside a stub nothing cleaned up. The
	// file is authoritative, exactly as a loose file wins over its sealed copy.
	const restored = "restored tarball bytes"

	root := buildArchive(t)
	tarball := evictTarball(t, root, "cv-9")
	writeFile(t, filepath.Join(root, "my-org"), tarball, restored)

	org := openOrg(t, root)

	entries, err := org.List(store.ConfigVersionsDirName)
	require.NoError(t, err)
	require.Equal(t, []string{tarball}, entryPaths(entries), "the object lists exactly once")

	e := entries[0]
	assert.False(t, e.Offloaded, "the bytes are here")
	assert.EqualValues(t, len(restored), e.Size, "sized from the file, not the stale stub")
	assert.False(t, e.ModTime.IsZero())
}

func TestOrgList_UnusableStubIsSkipped(t *testing.T) {
	t.Parallel()

	// A stub this build cannot trust reports nothing rather than a size it may
	// have misread, and never fails the listing around it.
	root := buildArchive(t)
	org := filepath.Join(root, "my-org")

	torn := store.ConfigVersionsDirName + "/cv-torn.tar.gz"
	writeFile(t, org, store.RemoteStubPath(torn), "{not json")

	future := store.ConfigVersionsDirName + "/cv-future.tar.gz"
	writeFile(t, org, store.RemoteStubPath(future),
		`{"version":9999,"size":12,"sha256":""}`)

	negative := store.ConfigVersionsDirName + "/cv-negative.tar.gz"
	writeFile(t, org, store.RemoteStubPath(negative),
		`{"version":1,"size":-4,"sha256":""}`)

	// A stub with no version field at all is not one any eviction wrote.
	unversioned := store.ConfigVersionsDirName + "/cv-unversioned.tar.gz"
	writeFile(t, org, store.RemoteStubPath(unversioned), `{}`)

	// A zero size is not a damaged stub, it is an empty object, and dropping it
	// would be the silent absence the stub exists to prevent.
	empty := store.ConfigVersionsDirName + "/cv-empty.tar.gz"
	writeFile(t, org, store.RemoteStubPath(empty), `{"version":1,"size":0,"sha256":""}`)

	readable := evictTarball(t, root, "cv-ok")

	entries, err := openOrg(t, root).List(store.ConfigVersionsDirName)
	require.NoError(t, err)
	assert.Equal(t, []string{empty, readable}, entryPaths(entries),
		"an unreadable stub drops its object, not the listing")

	e := entryByPath(t, entries, empty)
	assert.True(t, e.Offloaded)
	assert.Zero(t, e.Size, "an empty object lists at the size it has")
}

func TestOrgList_EntryMetadata(t *testing.T) {
	t.Parallel()

	org := openOrg(t, buildArchive(t))

	entries, err := org.List("")
	require.NoError(t, err)

	loose := entryByPath(t, entries, "org.json")
	assert.Equal(t, view.FormLoose, loose.Form)
	assert.Equal(t, "my-org", loose.Org)
	assert.Equal(t, "my-org/org.json", loose.ArchivePath())
	assert.Empty(t, loose.Container)
	assert.False(t, loose.Offloaded)
	assert.False(t, loose.ModTime.IsZero(), "a loose entry carries its mod time")
	assert.Positive(t, loose.Size)

	rollup := entryByPath(t, entries, cvPath)
	assert.Equal(t, view.FormRollup, rollup.Form)
	assert.Equal(t, cvRollup, rollup.Container, "the container is org-relative")
	assert.EqualValues(t, len(cvContent), rollup.Size, "a roll-up size is the content length")
	assert.True(t, rollup.ModTime.IsZero(), "a sealed entry has no mod time")
	assert.False(t, rollup.Offloaded)

	bundle := entryByPath(t, entries, planPath)
	assert.Equal(t, view.FormBundle, bundle.Form)
	assert.Equal(t, logsBundle, bundle.Container)
	assert.EqualValues(t, len(planContent), bundle.Size, "a bundle size comes from the sidecar")
	assert.False(t, bundle.Offloaded, "a local bundle is not offloaded")
}

func TestOrgList_LooseWinsOverSealed(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	writeFile(t, filepath.Join(root, "my-org"), cvPath, "loose survivor")

	org := openOrg(t, root)

	entries, err := org.List(wsDir + "/runs/run-new")
	require.NoError(t, err)

	e := entryByPath(t, entries, cvPath)
	assert.Equal(t, view.FormLoose, e.Form, "the loose survivor is the canonical copy")
	assert.EqualValues(t, len("loose survivor"), e.Size)

	// The object still lists exactly once.
	assert.Equal(t, []string{cvPath, planPath, wsDir + "/runs/run-new/run.json"}, entryPaths(entries))
}

func TestOrgList_OffloadedBundle(t *testing.T) {
	t.Parallel()

	// An evicted bundle: the zip is gone, its sidecar stays. The member still
	// lists with its recorded size, flagged as offloaded; the state bundle
	// remains local and unflagged.
	root := buildArchive(t)
	require.NoError(t, os.Remove(
		filepath.Join(root, "my-org", filepath.FromSlash(wsDir), "bundles", "logs.gen0001.zip")))

	org := openOrg(t, root)

	entries, err := org.List("")
	require.NoError(t, err)

	plan := entryByPath(t, entries, planPath)
	assert.True(t, plan.Offloaded, "a missing zip marks its members offloaded")
	assert.EqualValues(t, len(planContent), plan.Size, "the sidecar size works offline")

	blob := entryByPath(t, entries, svBlobPath)
	assert.False(t, blob.Offloaded, "the other bundle is still local")
}

func TestOrgRead_FullySealedDirectoryIsNotFile(t *testing.T) {
	t.Parallel()

	// Remove the loose run directory so the run survives only as sealed-index
	// keys: no physical directory, but archived objects beneath the path. The
	// upgrade to ErrNotFile is Org.Read's own contract, not an archive-layer
	// compensation.
	root := buildArchive(t)
	require.NoError(t, os.RemoveAll(
		filepath.Join(root, "my-org", filepath.FromSlash(wsDir), "runs", "run-new")))

	org := openOrg(t, root)

	_, err := org.Read(wsDir + "/runs/run-new")
	require.ErrorIs(t, err, view.ErrNotFile)
}

func TestOrgRead_EvictedTarballIsRemoteOnly(t *testing.T) {
	t.Parallel()

	// An evicted tarball is held by the archive, elsewhere. Saying "not found"
	// would send an operator looking for a collection bug; the answer they can
	// act on is where the bytes went.
	root := buildArchive(t)
	tarball := evictTarball(t, root, "cv-9")
	writeFile(t, filepath.Join(root, "my-org"), ".remote.json", viewMarker)

	_, err := openOrg(t, root).Read(tarball)
	require.ErrorIs(t, err, view.ErrRemoteOnly)
	assert.Contains(t, err.Error(), viewPrefix+"/my-org/"+tarball,
		"the message names the key to fetch")

	// Without a marker there is no key to name, but the object is no less
	// remote-only.
	bare := buildArchive(t)
	bareTarball := evictTarball(t, bare, "cv-9")

	_, err = openOrg(t, bare).Read(bareTarball)
	require.ErrorIs(t, err, view.ErrRemoteOnly)

	// A tarball with neither file nor stub was never archived here.
	_, err = openOrg(t, bare).Read(store.ConfigVersionsDirName + "/cv-absent.tar.gz")
	require.ErrorIs(t, err, view.ErrObjectNotFound)
}

func TestOrgRead(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		relPath string
		want    string
		err     error
	}{
		"loose file outside any workspace": {
			relPath: "org.json",
			want:    `{"data":{"id":"org-1","type":"organizations","attributes":{"name":"my-org"}}}`,
		},
		"rolled-up member": {
			relPath: cvPath,
			want:    cvContent,
		},
		"bundled member": {
			relPath: planPath,
			want:    planContent,
		},
		"directory": {
			relPath: wsDir,
			err:     view.ErrNotFile,
		},
		"empty path": {
			relPath: "",
			err:     view.ErrNotFile,
		},
		"missing object": {
			relPath: wsDir + "/runs/run-new/apply.log",
			err:     view.ErrObjectNotFound,
		},
		"traversal is refused": {
			relPath: "../escape",
			err:     view.ErrInvalidPath,
		},
		"bare slash is refused": {
			relPath: "/",
			err:     view.ErrInvalidPath,
		},
		"machinery is readable by its physical path": {
			// Read deliberately applies no machinery filter: the roll-up file
			// List hides is still inspectable raw.
			relPath: cvRollup,
			want:    "", // checked below by substring; NDJSON carries a digest
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			org := openOrg(t, buildArchive(t))

			data, err := org.Read(tt.relPath)
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)

				return
			}

			require.NoError(t, err)

			if tt.want != "" {
				assert.Equal(t, tt.want, string(data))
			} else {
				assert.Contains(t, string(data), "cv-1",
					"the raw roll-up line is served verbatim")
			}
		})
	}
}
