package view_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/seal"
	"go.jacobcolvin.com/hcp_archiver/pkg/view"
)

// buildStatesArchive lays out one organization whose fixture workspace holds
// the given state-version roll-up lines and loose files, then packs the named
// loose paths into a bundle (which removes them, the way a seal does) and
// returns the opened workspace.
func buildStatesArchive(t *testing.T, rollup string, loose map[string]string, bundle []string) *view.Workspace {
	t.Helper()

	root := t.TempDir()
	org := filepath.Join(root, "my-org")

	writeFile(t, org, "org.json",
		`{"data":{"id":"org-1","type":"organizations","attributes":{"name":"my-org"}}}`)
	writeFile(t, org, wsDir+"/workspace.json",
		`{"data":{"id":"ws-1","type":"workspaces","attributes":{"name":"app"}}}`)

	if rollup != "" {
		writeFile(t, org, wsDir+"/rollups/state-versions.ndjson", rollup)
	}

	sources := make(map[string]string, len(loose))
	for rel, content := range loose {
		sources[rel] = writeFile(t, org, rel, content)
	}

	members := make([]seal.Member, 0, len(bundle))
	for _, rel := range bundle {
		require.Contains(t, sources, rel, "a bundled member is written loose first")

		members = append(members, seal.Member{Name: rel, Source: sources[rel]})
	}

	_, err := seal.Seal(filepath.Join(org, filepath.FromSlash(wsDir), "bundles", "state.gen0001.zip"), members)
	require.NoError(t, err)

	return openOrg(t, root).Workspace("default", "app")
}

func TestStateVersionsAcrossForms(t *testing.T) {
	t.Parallel()

	// A version's metadata and its blobs are sealed independently, so every
	// combination occurs in a swept archive. The summary reads the sidecar and
	// reports which blobs the workspace holds from the one listing that named
	// them, so a blob in either form must count and one in neither must not.
	const stem = "20240101T000000Z-sv-1"

	svDir := wsDir + "/state-versions/"
	meta := rollupRecord(t, svDir+stem+".meta.json", metaJSON("sv-1", "2", "2024-01-01T00:00:00Z"))

	tests := map[string]struct {
		rollup   string
		loose    map[string]string
		bundle   []string
		wantID   string
		wantRaw  bool
		wantJSON bool
	}{
		"meta rolled up, raw blob loose": {
			rollup:  meta,
			loose:   map[string]string{svDir + stem + ".tfstate.json": `{"serial":2}`},
			wantID:  "sv-1",
			wantRaw: true,
		},
		"meta loose, raw blob bundled": {
			loose: map[string]string{
				svDir + stem + ".meta.json":    metaJSON("sv-1", "2", "2024-01-01T00:00:00Z"),
				svDir + stem + ".tfstate.json": `{"serial":2}`,
			},
			bundle:  []string{svDir + stem + ".tfstate.json"},
			wantID:  "sv-1",
			wantRaw: true,
		},
		"meta rolled up, both blobs bundled": {
			rollup: meta,
			loose: map[string]string{
				svDir + stem + ".tfstate.json": `{"serial":2}`,
				svDir + stem + ".json":         `{"format_version":"1.0"}`,
			},
			bundle:   []string{svDir + stem + ".tfstate.json", svDir + stem + ".json"},
			wantID:   "sv-1",
			wantRaw:  true,
			wantJSON: true,
		},
		"meta rolled up, no blob at all": {
			rollup: meta,
			wantID: "sv-1",
		},
		"meta unreadable, blob loose": {
			// A sidecar present but unparsable: the version still lists off its
			// stem, with its parsed fields zero.
			loose: map[string]string{
				svDir + stem + ".meta.json":    "not json",
				svDir + stem + ".tfstate.json": `{"serial":2}`,
			},
			wantRaw: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ws := buildStatesArchive(t, tt.rollup, tt.loose, tt.bundle)

			versions, err := ws.StateVersions()
			require.NoError(t, err)
			require.Len(t, versions, 1)

			assert.Equal(t, stem, versions[0].Stem)
			assert.Equal(t, tt.wantID, versions[0].ID)
			assert.Equal(t, tt.wantRaw, versions[0].HasRaw, "raw blob presence")
			assert.Equal(t, tt.wantJSON, versions[0].HasJSON, "json blob presence")
		})
	}
}

func TestStateVersionsSealedBlobIsReadable(t *testing.T) {
	t.Parallel()

	// Presence is answered from a listing rather than a probe, so it has to
	// keep agreeing with the read: a blob reported present must open.
	const stem = "20240101T000000Z-sv-1"

	svDir := wsDir + "/state-versions/"

	ws := buildStatesArchive(t,
		rollupRecord(t, svDir+stem+".meta.json", metaJSON("sv-1", "2", "2024-01-01T00:00:00Z")),
		map[string]string{svDir + stem + ".tfstate.json": `{"serial":2}`},
		[]string{svDir + stem + ".tfstate.json"})

	versions, err := ws.StateVersions()
	require.NoError(t, err)
	require.Len(t, versions, 1)
	require.True(t, versions[0].HasRaw)

	data, err := ws.Open(ws.RawStatePath(&versions[0]))
	require.NoError(t, err)
	assert.JSONEq(t, `{"serial":2}`, string(data))
}
