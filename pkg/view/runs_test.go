package view_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/view"
)

// buildRunsArchive lays out one organization whose fixture workspace has the
// given raw NDJSON in rollups/runs.ndjson and any extra archive-relative
// files, then returns the opened workspace. With no extras there is no runs/
// directory at all: the fully coalesced shape.
func buildRunsArchive(t *testing.T, runsNDJSON string, extras map[string]string) *view.Workspace {
	t.Helper()

	root := t.TempDir()
	org := filepath.Join(root, "my-org")

	writeFile(t, org, "org.json",
		`{"data":{"id":"org-1","type":"organizations","attributes":{"name":"my-org"}}}`)
	writeFile(t, org, wsDir+"/workspace.json",
		`{"data":{"id":"ws-1","type":"workspaces","attributes":{"name":"app"}}}`)
	writeFile(t, org, wsDir+"/rollups/runs.ndjson", runsNDJSON)

	for rel, content := range extras {
		writeFile(t, org, rel, content)
	}

	orgs, err := view.OpenArchive(root)
	require.NoError(t, err)
	require.Len(t, orgs, 1)

	return orgs[0].Workspace("default", "app")
}

// rollupRecord renders one roll-up record carrying content at an
// archive-relative path, encoded the way seal.Rollup writes it.
func rollupRecord(t *testing.T, path, content string) string {
	t.Helper()

	line, err := json.Marshal(struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{Path: path, Content: content})
	require.NoError(t, err)

	return string(line) + "\n"
}

// runsLine renders one runs.ndjson roll-up record carrying a run document.
func runsLine(t *testing.T, id, status, createdAt string) string {
	t.Helper()

	return rollupRecord(t, wsDir+"/runs/"+id+"/run.json", runJSON(id, status, createdAt))
}

func TestWorkspaceRunsFromRollupOnly(t *testing.T) {
	t.Parallel()

	// A fully coalesced workspace: every run terminal, no runs/ directory.
	ws := buildRunsArchive(t,
		runsLine(t, "run-a", "applied", "2024-01-01T10:00:00Z")+
			runsLine(t, "run-b", "errored", "2024-02-01T10:00:00Z"),
		nil)

	runs, err := ws.Runs()
	require.NoError(t, err)
	require.Len(t, runs, 2)

	assert.Equal(t, "run-b", runs[0].ID, "newest first")
	assert.Equal(t, "errored", runs[0].Status)
	assert.Equal(t, "run-a", runs[1].ID)
	assert.Equal(t, "applied", runs[1].Status, "fields parse from the roll-up line")
}

func TestWorkspaceRunsMixedLooseAndCoalesced(t *testing.T) {
	t.Parallel()

	ws := buildRunsArchive(t,
		runsLine(t, "run-sealed", "applied", "2024-01-01T10:00:00Z"),
		map[string]string{
			wsDir + "/runs/run-live/run.json": runJSON("run-live", "planning", "2024-03-01T10:00:00Z"),
		})

	runs, err := ws.Runs()
	require.NoError(t, err)
	require.Len(t, runs, 2)

	assert.Equal(t, "run-live", runs[0].ID, "the loose in-flight run lists beside the sealed one")
	assert.Equal(t, "planning", runs[0].Status)
	assert.Equal(t, "run-sealed", runs[1].ID)
	assert.Equal(t, "applied", runs[1].Status)
}

func TestWorkspaceRunsDuplicateRollupLinesNewestWins(t *testing.T) {
	t.Parallel()

	// The same run re-frozen after its content changed appends a newer line
	// under the same path; the reader must serve the newest.
	ws := buildRunsArchive(t,
		runsLine(t, "run-a", "canceled", "2024-01-01T10:00:00Z")+
			runsLine(t, "run-a", "force_canceled", "2024-01-01T10:00:00Z"),
		nil)

	runs, err := ws.Runs()
	require.NoError(t, err)
	require.Len(t, runs, 1)

	assert.Equal(t, "force_canceled", runs[0].Status, "the newest line is canonical")
}

func TestWorkspaceRunsCorruptRollupLineStillListsViaChildren(t *testing.T) {
	t.Parallel()

	// Run-x's own run.json line is corrupt (skipped by the index), but a
	// sealed child under the same run keeps the id visible; its parsed fields
	// stay zero.
	corrupt := `{"path":"` + wsDir + `/runs/run-x/run.json","content": not-json` + "\n"
	child := rollupRecord(t, wsDir+"/runs/run-x/comments.json", "[]")

	ws := buildRunsArchive(t, corrupt+child, nil)

	runs, err := ws.Runs()
	require.NoError(t, err)
	require.Len(t, runs, 1)

	assert.Equal(t, "run-x", runs[0].ID, "a damaged summary line does not hide the run")
	assert.Empty(t, runs[0].Status)
}
