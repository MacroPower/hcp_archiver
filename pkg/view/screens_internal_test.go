package view

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/list"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tea "charm.land/bubbletea/v2"
)

// testWorkspaceDir is the archive-relative directory of the one workspace
// [newTestWorkspace] writes.
const testWorkspaceDir = "projects/default/workspaces/app"

// newTestWorkspace writes a minimal archive whose one workspace's
// workspace.json holds content, plus any extra files given as
// archive-relative path to content, and returns that workspace.
func newTestWorkspace(t *testing.T, workspaceJSON string, extra map[string]string) *Workspace {
	t.Helper()

	root := t.TempDir()
	org := filepath.Join(root, "my-org")

	write := func(rel, content string) {
		abs := filepath.Join(org, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))
	}

	write("org.json", `{"data":{"id":"org-1","type":"organizations"}}`)
	write(testWorkspaceDir+"/workspace.json", workspaceJSON)

	for rel, content := range extra {
		write(rel, content)
	}

	orgs, err := OpenArchive(root)
	require.NoError(t, err)
	require.Len(t, orgs, 1)

	return orgs[0].Workspace("default", "app")
}

func TestNewWorkspaceScreenCountsCoalescedRuns(t *testing.T) {
	t.Parallel()

	// A fully coalesced workspace has no runs/ directory at all; the screen's
	// run count must come from the same loose-plus-sealed enumeration the run
	// list uses, not from counting subdirectories.
	ws := newTestWorkspace(t, `{"data":{"id":"ws-1","type":"workspaces"}}`, map[string]string{
		testWorkspaceDir + "/rollups/runs.ndjson": `{"path":"` + testWorkspaceDir + `/runs/run-a/run.json","content":"{}"}` + "\n" +
			`{"path":"` + testWorkspaceDir + `/runs/run-b/run.json","content":"{}"}` + "\n",
	})

	screen, err := newWorkspaceScreen(ws)
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

func TestNewOverviewScreenRendersStatsTable(t *testing.T) {
	t.Parallel()

	ws := newTestWorkspace(t, `{"data":{"id":"ws-1","type":"workspaces","attributes":{
		"description":"the app",
		"terraform-version":"1.9.4",
		"auto-apply":true,
		"resource-count":12,
		"vcs-repo":{"identifier":"acme/app"}
	}}}`, nil)

	s, err := newOverviewScreen(ws)
	require.NoError(t, err)

	tv, ok := s.(*tableViewerScreen)
	require.True(t, ok, "a single-resource workspace.json gets the stats table")

	tv.setSize(80, 24)

	frame := tv.view()
	assert.Contains(t, frame, "terraform version")
	assert.Contains(t, frame, "1.9.4")
	assert.Contains(t, frame, "attributes", "the highlighted JSON body renders below the table")
	assert.Contains(t, frame, "w wrap")
	assert.Contains(t, frame, "esc back")
}

func TestNewOverviewScreenFallsBackToRawDocument(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		workspaceJSON string
	}{
		"malformed JSON": {
			workspaceJSON: `{not json`,
		},
		"two-resource list": {
			workspaceJSON: `{"data":[{"id":"ws-1","type":"workspaces"},{"id":"ws-2","type":"workspaces"}]}`,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ws := newTestWorkspace(t, tc.workspaceJSON, nil)

			s, err := newOverviewScreen(ws)
			require.NoError(t, err)

			assert.IsType(t, &yamlViewerScreen{}, s, "the raw document still displays")
		})
	}
}

func TestListScreenBackKeysClearAnAppliedFilter(t *testing.T) {
	t.Parallel()

	// The list binds only esc to clearing an applied filter; the screen must
	// handle backspace itself or it dies as a no-op, breaking the documented
	// contract that esc and backspace are interchangeable back keys.
	for _, backKey := range []tea.Key{{Code: tea.KeyEscape}, {Code: tea.KeyBackspace}} {
		t.Run(tea.KeyPressMsg(backKey).String(), func(t *testing.T) {
			t.Parallel()

			s := newListScreen("test", []item{
				{title: "alpha", desc: "row"},
				{title: "beta", desc: "row"},
			})
			s.setSize(80, 24)

			press := func(k tea.Key) tea.Cmd { return s.update(tea.KeyPressMsg(k)) }

			press(tea.Key{Code: '/', Text: "/"})
			press(tea.Key{Code: 'a', Text: "a"})
			press(tea.Key{Code: tea.KeyEnter})
			require.Equal(t, list.FilterApplied, s.list.FilterState())

			// The first press clears the filter without popping the screen; only
			// the second, on the now-unfiltered list, pops.
			assert.Nil(t, press(backKey))
			assert.Equal(t, list.Unfiltered, s.list.FilterState())
			assert.NotNil(t, press(backKey))
		})
	}
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
