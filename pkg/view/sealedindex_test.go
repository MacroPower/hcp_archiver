package view_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/view"
)

// buildSealedOrg lays out one organization whose fixture workspace has the
// given raw NDJSON in rollups/runs.ndjson and nothing loose beneath runs/,
// then returns the opened organization.
func buildSealedOrg(t *testing.T, runsNDJSON string) *view.Org {
	t.Helper()

	root := t.TempDir()
	org := filepath.Join(root, "my-org")

	writeFile(t, org, "org.json",
		`{"data":{"id":"org-1","type":"organizations","attributes":{"name":"my-org"}}}`)
	writeFile(t, org, wsDir+"/workspace.json",
		`{"data":{"id":"ws-1","type":"workspaces","attributes":{"name":"app"}}}`)
	writeFile(t, org, wsDir+"/rollups/runs.ndjson", runsNDJSON)

	return openOrg(t, root)
}

func TestListEmptyPrefixHoldsEverySealedObject(t *testing.T) {
	t.Parallel()

	// The whole-organization listing is the common path, and it asks the sealed
	// index for the empty prefix. A prefix scan that seeks to prefix+"/" finds
	// nothing for the empty prefix, so it has to answer the whole index
	// instead; getting that wrong makes every sealed object vanish from the
	// listing, the inspect commands, and every extract.
	org := openOrg(t, buildArchive(t))

	entries, err := org.List("")
	require.NoError(t, err)

	assert.Subset(t, entryPaths(entries), []string{
		cvPath,
		planPath,
		wsDir + "/state-versions/20240101T000000Z-sv-1.meta.json",
		wsDir + "/state-versions/20240101T000000Z-sv-1.tfstate.json",
	}, "every sealed form lists under the empty prefix")
}

func TestSealedPrefixMatchesWholeSegments(t *testing.T) {
	t.Parallel()

	// '!' and '-' sort below '/', so "runs!x" and "runs-x" fall between "runs"
	// and "runs/a" in the sorted index. A prefix scan that seeks to the bare
	// directory name lands on them and stops on the first, dropping the runs
	// subtree entirely; one that seeks past the separator skips them.
	org := buildSealedOrg(t,
		rollupRecord(t, wsDir+"/runs!x", "sibling")+
			rollupRecord(t, wsDir+"/runs-x", "sibling")+
			runsLine(t, "run-a", "applied", "2024-01-01T10:00:00Z")+
			rollupRecord(t, wsDir+"/runsz", "sibling"))

	runs, err := org.Workspace("default", "app").Runs()
	require.NoError(t, err)
	require.Len(t, runs, 1, "the neighbors are not runs, and do not hide the run either")
	assert.Equal(t, "run-a", runs[0].ID)

	entries, err := org.List(wsDir + "/runs")
	require.NoError(t, err)
	assert.Equal(t, []string{wsDir + "/runs/run-a/run.json"}, entryPaths(entries),
		"only the subtree lists under the directory prefix")

	// The neighbors are still objects in their own right.
	assert.Subset(t, entryPaths(mustList(t, org, "")),
		[]string{wsDir + "/runs!x", wsDir + "/runs-x", wsDir + "/runsz"})
}

func TestSealedKeyNamingDirectoryItself(t *testing.T) {
	t.Parallel()

	// A corrupt record naming a run directory rather than a file: the id still
	// lists (any sealed child keeps a run visible), the key contributes no
	// artifact leaf, and reading the directory is not a file.
	org := buildSealedOrg(t, rollupRecord(t, wsDir+"/runs/run-z", "directory"))
	ws := org.Workspace("default", "app")

	runs, err := ws.Runs()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "run-z", runs[0].ID)
	assert.Empty(t, runs[0].Status, "no run.json to parse")

	artifacts, err := ws.RunArtifacts("run-z")
	require.NoError(t, err)
	assert.Empty(t, artifacts, "the key names the directory, not a leaf inside it")

	_, err = org.Read(wsDir + "/runs")
	require.ErrorIs(t, err, view.ErrNotFile, "the run directory holds archived objects")
}

// mustList returns the organization's entries at a prefix.
func mustList(t *testing.T, org *view.Org, prefix string) []view.Entry {
	t.Helper()

	entries, err := org.List(prefix)
	require.NoError(t, err)

	return entries
}
