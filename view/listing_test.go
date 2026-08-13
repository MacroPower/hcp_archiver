package view_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	// sidecars, roll-ups, a ledger shard, a staging leftover, plus an
	// org-root remote marker this test adds.
	root := buildUnsealArchive(t)
	writeFile(t, filepath.Join(root, "my-org"), ".remote.json", viewMarker)

	org := openOrg(t, root)

	entries, err := org.List("")
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	for _, p := range entryPaths(entries) {
		assert.NotContains(t, p, "/bundles/", "bundle zips and sidecars are hidden: %s", p)
		assert.NotContains(t, p, "/rollups/", "roll-up files are hidden: %s", p)
		assert.NotContains(t, p, ".sidecar", "sidecar indexes are hidden: %s", p)
		assert.NotContains(t, p, ".ledger", "ledger shards are hidden: %s", p)
		assert.NotContains(t, p, ".remote.json", "the remote marker is hidden: %s", p)
		assert.NotContains(t, p, ".atomicfile-", "staging leftovers are hidden: %s", p)
	}
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
