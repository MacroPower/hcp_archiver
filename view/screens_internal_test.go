package view

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewWorkspaceScreenCountsCoalescedRuns(t *testing.T) {
	t.Parallel()

	// A fully coalesced workspace has no runs/ directory at all; the screen's
	// run count must come from the same loose-plus-sealed enumeration the run
	// list uses, not from counting subdirectories.
	root := t.TempDir()
	org := filepath.Join(root, "my-org")
	ws := "projects/default/workspaces/app"

	write := func(rel, content string) {
		abs := filepath.Join(org, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))
	}

	write("org.json", `{"data":{"id":"org-1","type":"organizations"}}`)
	write(ws+"/workspace.json", `{"data":{"id":"ws-1","type":"workspaces"}}`)
	write(ws+"/rollups/runs.ndjson",
		`{"path":"`+ws+`/runs/run-a/run.json","content":"{}"}`+"\n"+
			`{"path":"`+ws+`/runs/run-b/run.json","content":"{}"}`+"\n")

	orgs, err := OpenArchive(root)
	require.NoError(t, err)
	require.Len(t, orgs, 1)

	screen, err := newWorkspaceScreen(orgs[0].Workspace("default", "app"))
	require.NoError(t, err)

	ls, ok := screen.(*listScreen)
	require.True(t, ok)

	var runsDesc string

	for _, entry := range ls.list.Items() {
		row, isItem := entry.(item)
		require.True(t, isItem)

		if row.title == "Runs" {
			runsDesc = row.desc
		}
	}

	assert.Equal(t, "2 runs", runsDesc, "the count matches the coalesced run list")
}
