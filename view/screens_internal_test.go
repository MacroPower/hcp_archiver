package view

import (
	"os"
	"path/filepath"
	"strings"
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

func TestFirstLine(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		text string
		want string
	}{
		"single line passes through": {
			text: "Update infra",
			want: "Update infra",
		},
		"LF cuts to the first line": {
			text: "Update infra\nMore details",
			want: "Update infra",
		},
		"CRLF leaves no trailing carriage return": {
			text: "Update infra\r\nMore details",
			want: "Update infra",
		},
		"bare CR cuts to the first line": {
			text: "Update infra\rMore details",
			want: "Update infra",
		},
		"long line truncates on a rune boundary": {
			text: strings.Repeat("ü", 61),
			want: strings.Repeat("ü", 60) + "…",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, firstLine(tc.text))
		})
	}
}
