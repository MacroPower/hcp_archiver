package view_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/remote/remotetest"
	"go.jacobcolvin.com/hcp_archiver/view"
)

// mergeProject holds the workspaces directory whose merged children the
// collision cases inspect, and mergeDir is that directory.
const mergeProject = "p"

// mergeDir is the archive-relative directory the collision cases list.
const mergeDir = "projects/" + mergeProject + "/workspaces"

// mergeBody is what every collision fixture writes, on disk and in the mirror
// alike, so each merged entry carries the same size.
const mergeBody = "{}"

// orgBody is a minimal organization document, enough to make a directory an
// organization root on either side of the merge.
const orgBody = `{"data":{"id":"org-1","type":"organizations","attributes":{"name":"my-org"}}}`

// openMerged lays out one local organization tree and a mirror inventory over
// the same organization, then opens the archive across both. Local paths are
// files, so a local directory is named by writing a file beneath it; mirror
// keys are archive-relative and joined under the organization's prefix.
func openMerged(t *testing.T, local, mirror []string) *view.Org {
	t.Helper()

	root := t.TempDir()
	orgRoot := filepath.Join(root, "my-org")

	writeFile(t, orgRoot, "org.json", orgBody)

	for _, rel := range local {
		writeFile(t, orgRoot, rel, mergeBody)
	}

	fake := remotetest.New()
	fake.SetObject(viewPrefix+"/my-org/org.json", remotetest.Object{Data: []byte(orgBody)})

	for _, key := range mirror {
		fake.SetObject(viewPrefix+"/my-org/"+key, remotetest.Object{Data: []byte(mergeBody)})
	}

	orgs, err := view.OpenArchive(root,
		view.WithContext(t.Context()),
		view.WithRemote(viewRemoteConfig()),
		view.WithRemoteFactory(fakeFactory(fake)),
	)
	require.NoError(t, err)
	require.Len(t, orgs, 1)

	return orgs[0]
}

func TestEntries_HidesMachineryAndFoldsStubs(t *testing.T) {
	t.Parallel()

	const (
		cvDir    = "config-versions"
		stubBody = `{"sha256":"","size":123,"version":1}`
		wsDir    = mergeDir + "/app"
	)

	tests := map[string]struct {
		// The files written under the organization root, keyed by
		// archive-relative path.
		local map[string]string
		// The archive-relative keys the mirror holds.
		mirror []string
		// The directory whose merged children are listed.
		dir string
		// The merged listing, directories first.
		want []view.TreeEntry
	}{
		"machinery is hidden on both sides": {
			local: map[string]string{
				wsDir + "/workspace.json":        mergeBody,
				wsDir + "/.identity.json":        `{"id":"ws-1"}`,
				wsDir + "/.atomicfile-1234.tmp":  "half-written",
				wsDir + "/.ledger/snapshot.json": `{"version":2}`,
				wsDir + "/bundles/b1.zip":        "zip",
			},
			mirror: []string{
				wsDir + "/.identity.json",
				wsDir + "/rollups/runs.ndjson",
			},
			dir:  wsDir,
			want: []view.TreeEntry{{Name: "workspace.json", Size: int64(len(mergeBody))}},
		},
		"a workspace named bundles still lists": {
			// The machinery match is positional, so a workspace whose own name
			// collides with the sealed-form directory names is not machinery.
			local: map[string]string{mergeDir + "/bundles/workspace.json": mergeBody},
			dir:   mergeDir,
			want:  []view.TreeEntry{{Name: "bundles", Dir: true}},
		},
		"a stub alone folds onto its target": {
			local: map[string]string{cvDir + "/cv-1.tar.gz.remote.json": stubBody},
			dir:   cvDir,
			want:  []view.TreeEntry{{Name: "cv-1.tar.gz", Size: 123, Remote: true}},
		},
		"a local target wins over its stub": {
			local: map[string]string{
				cvDir + "/cv-1.tar.gz":             "tarball bytes",
				cvDir + "/cv-1.tar.gz.remote.json": stubBody,
			},
			dir:  cvDir,
			want: []view.TreeEntry{{Name: "cv-1.tar.gz", Size: int64(len("tarball bytes"))}},
		},
		"a stub's record wins over the mirror's target key": {
			local:  map[string]string{cvDir + "/cv-1.tar.gz.remote.json": stubBody},
			mirror: []string{cvDir + "/cv-1.tar.gz"},
			dir:    cvDir,
			want:   []view.TreeEntry{{Name: "cv-1.tar.gz", Size: 123, Remote: true}},
		},
		"a damaged stub drops entirely": {
			local: map[string]string{cvDir + "/cv-1.tar.gz.remote.json": `{"version":99,"size":1}`},
			dir:   cvDir,
			want:  []view.TreeEntry{},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			orgRoot := filepath.Join(root, "my-org")

			writeFile(t, orgRoot, "org.json", orgBody)

			for rel, content := range tc.local {
				writeFile(t, orgRoot, rel, content)
			}

			fake := remotetest.New()
			fake.SetObject(viewPrefix+"/my-org/org.json", remotetest.Object{Data: []byte(orgBody)})

			for _, key := range tc.mirror {
				fake.SetObject(viewPrefix+"/my-org/"+key, remotetest.Object{Data: []byte(mergeBody)})
			}

			orgs, err := view.OpenArchive(root,
				view.WithContext(t.Context()),
				view.WithRemote(viewRemoteConfig()),
				view.WithRemoteFactory(fakeFactory(fake)),
			)
			require.NoError(t, err)
			require.Len(t, orgs, 1)

			entries, err := orgs[0].Entries(tc.dir)
			require.NoError(t, err)
			assert.Equal(t, tc.want, entries)
		})
	}
}

func TestEntries_LocalFormWinsOverTheMirrorRecord(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		// The files written under the organization root; a local directory is
		// named by writing a file beneath it.
		local []string
		// The archive-relative keys the mirror holds.
		mirror []string
		// The merged listing of mergeDir, directories first.
		want []view.TreeEntry
		// What the directory-only enumeration answers there: the workspace
		// names a browser offers to descend into.
		subdirs []string
	}{
		"mirror subtree under a local file": {
			local:  []string{mergeDir + "/foo"},
			mirror: []string{mergeDir + "/foo/workspace.json"},
			want:   []view.TreeEntry{{Name: "foo", Size: int64(len(mergeBody))}},
		},
		"mirror file over a local directory": {
			local:   []string{mergeDir + "/bar/workspace.json"},
			mirror:  []string{mergeDir + "/bar"},
			want:    []view.TreeEntry{{Name: "bar", Dir: true}},
			subdirs: []string{"bar"},
		},
		"distinct names union": {
			local:  []string{mergeDir + "/loose.json"},
			mirror: []string{mergeDir + "/remote-ws/workspace.json"},
			want: []view.TreeEntry{
				{Name: "remote-ws", Dir: true},
				{Name: "loose.json", Size: int64(len(mergeBody))},
			},
			subdirs: []string{"remote-ws"},
		},
		"both sides hold the same directory": {
			local:   []string{mergeDir + "/app/workspace.json"},
			mirror:  []string{mergeDir + "/app/variables.json"},
			want:    []view.TreeEntry{{Name: "app", Dir: true}},
			subdirs: []string{"app"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			org := openMerged(t, tc.local, tc.mirror)

			entries, err := org.Entries(mergeDir)
			require.NoError(t, err)
			assert.Equal(t, tc.want, entries)

			seen := make(map[string]int, len(entries))
			for _, e := range entries {
				seen[e.Name]++
			}

			for entryName, count := range seen {
				assert.Equal(t, 1, count, "%q lists once", entryName)
			}

			names, err := org.Workspaces(mergeProject)
			require.NoError(t, err)
			assert.Equal(t, tc.subdirs, names)
		})
	}
}

// linkedOrgDir prepares an archive root whose organization directory is
// reached through a symlink, so the root's own scan skips it (a symlink is no
// directory entry) and the organization's name arrives from the mirror alone,
// putting the local directory in front of the materialization rather than in
// front of the local org-root scan. It returns the archive root and the
// directory the link resolves to.
func linkedOrgDir(t *testing.T) (string, string) {
	t.Helper()

	base := t.TempDir()
	root := filepath.Join(base, "archive")
	target := filepath.Join(base, "elsewhere", "my-org")

	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.MkdirAll(target, 0o755))
	require.NoError(t, os.Symlink(target, filepath.Join(root, "my-org")))

	return root, target
}

func TestList_UncleanMirrorKeysAreDropped(t *testing.T) {
	t.Parallel()

	// A bucket key is arbitrary bytes. One carrying dot segments would surface
	// a listed entry every read and extract then refuses (counting it errored
	// on every recovery), and index a ".." child whose joined path escapes the
	// directory it lists under. Such keys are dropped where the inventory is
	// ingested, so no consumer sees them.
	org := openMerged(t,
		[]string{mergeDir + "/app/workspace.json"},
		[]string{
			mergeDir + "/../escape.json",
			mergeDir + "/./self.json",
			mergeDir + "/app/ok.json",
		},
	)

	entries, err := org.List("")
	require.NoError(t, err)

	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}

	assert.Contains(t, paths, mergeDir+"/app/ok.json", "the clean mirror key still lists")
	assert.NotContains(t, paths, mergeDir+"/../escape.json")
	assert.NotContains(t, paths, mergeDir+"/./self.json")

	tree, err := org.Entries(mergeDir)
	require.NoError(t, err)

	for _, te := range tree {
		assert.NotEqual(t, "..", te.Name, "no phantom parent-directory child")
		assert.NotEqual(t, ".", te.Name, "no phantom self child")
	}
}

func TestOpenArchive_OrgRootStatFaultRefuses(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		// Seeds the organization's local directory before the open.
		prepare func(t *testing.T, dir string)
		// The substring the open's refusal must carry; an empty one expects
		// the organization to materialize instead.
		err string
	}{
		"absent org.json materializes": {
			prepare: func(*testing.T, string) {},
		},
		"unreadable org.json refuses": {
			prepare: func(t *testing.T, dir string) {
				t.Helper()

				// A self-referential symlink: every stat of it resolves to
				// itself and faults with ELOOP, which is neither the file
				// being present nor it being absent.
				require.NoError(t, os.Symlink("org.json", filepath.Join(dir, "org.json")))
			},
			err: `materialize organization "my-org"`,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root, orgDir := linkedOrgDir(t)
			tc.prepare(t, orgDir)

			fake := remotetest.New()
			fake.SetObject(viewPrefix+"/my-org/org.json", remotetest.Object{Data: []byte(orgBody)})

			orgs, err := view.OpenArchive(root,
				view.WithContext(t.Context()),
				view.WithRemote(viewRemoteConfig()),
				view.WithRemoteFactory(fakeFactory(fake)),
			)

			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				assert.NoFileExists(t, filepath.Join(orgDir, remote.MarkerName),
					"an organization that could not be read is not marked materialized")

				return
			}

			require.NoError(t, err)
			require.Len(t, orgs, 1)
			assert.FileExists(t, filepath.Join(orgDir, "org.json"))
			assert.FileExists(t, filepath.Join(orgDir, remote.MarkerName))
		})
	}
}
