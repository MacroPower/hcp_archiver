package view

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tea "charm.land/bubbletea/v2"
)

// testTableViewer builds a three-row screen over a JSON document long enough
// to scroll at every size the tests use.
func testTableViewer() *tableViewerScreen {
	var b strings.Builder

	b.WriteString("{\n")

	for i := range 40 {
		fmt.Fprintf(&b, "  \"key-%d\": %d,\n", i, i)
	}

	b.WriteString("  \"last\": true\n}\n")

	cols := []table.Column{
		{Title: "", Width: 12},
		{Title: "", Width: 1},
	}
	rows := []table.Row{
		{"alpha", "one"},
		{"beta", "two"},
		{"gamma", "three"},
	}

	return newTableViewerScreen("stats", cols, rows, b.String())
}

func TestTableViewerScreenSizing(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		w int
		h int
	}{
		"tall":                 {w: 80, h: 24},
		"minimum with table":   {w: 80, h: 4},
		"table clipped":        {w: 40, h: 5},
		"table hidden":         {w: 80, h: 3},
		"viewport only":        {w: 80, h: 2},
		"footer only":          {w: 80, h: 1},
		"zero height":          {w: 80, h: 0},
		"narrow flexed column": {w: 5, h: 24},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := testTableViewer()
			s.setSize(tc.w, tc.h)

			frame := s.view()

			if tc.h >= 4 {
				assert.Equal(t, tc.h, lipgloss.Height(frame), "the frame fills the body exactly")
			} else {
				// Degenerate heights follow the same floor as the other
				// viewers: the footer line plus whatever the viewport still
				// emits, never more than two lines over the budget.
				assert.LessOrEqual(t, lipgloss.Height(frame), max(tc.h, 2))
				assert.True(t, s.tableHidden, "the table disappears before the viewport does")
			}
		})
	}
}

func TestTableViewerScreenTableStaysCompact(t *testing.T) {
	t.Parallel()

	// At a tall size the table takes its natural height (header plus rows) and
	// no more, so no blank filler opens up between it and the document.
	s := testTableViewer()
	s.setSize(80, 24)

	assert.Equal(t, 1+len(s.table.Rows()), lipgloss.Height(s.table.View()))
	assert.Equal(t, 24, lipgloss.Height(s.view()))
}

func TestTableViewerScreenContent(t *testing.T) {
	t.Parallel()

	s := testTableViewer()
	s.setSize(80, 24)

	assert.Equal(t, "stats", s.crumb())

	frame := s.view()
	assert.Contains(t, frame, "alpha")
	assert.Contains(t, frame, "one")
	assert.Contains(t, frame, "gamma")
	assert.Contains(t, frame, "three")
	assert.Contains(t, frame, "key-0", "the JSON document renders below the table")
	assert.Contains(t, frame, "w wrap")
	assert.Contains(t, frame, "esc back")
}

func TestTableViewerScreenLongValueTruncates(t *testing.T) {
	t.Parallel()

	cols := []table.Column{
		{Title: "", Width: 8},
		{Title: "", Width: 1},
	}
	rows := []table.Row{{"label", strings.Repeat("v", 100)}}

	s := newTableViewerScreen("stats", cols, rows, "{}")
	s.setSize(30, 10)

	frame := s.view()
	assert.Contains(t, frame, "label")
	assert.Contains(t, frame, "…", "an overlong value truncates instead of dropping the column")
}

func TestTableViewerScreenTableIsStatic(t *testing.T) {
	t.Parallel()

	s := testTableViewer()
	s.setSize(80, 10)

	require.Zero(t, s.vp.ScrollPercent())

	cmd := s.update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	assert.Nil(t, cmd)
	assert.Positive(t, s.vp.ScrollPercent(), "a scroll key drives the viewport, not the table")
	assert.Zero(t, s.table.Cursor(), "the table never sees a key")

	// The static look: no row of the rendered table carries selection
	// styling, so every row line is plain text. Only the header line (the
	// first) is styled, by the heading tone.
	lines := strings.Split(s.table.View(), "\n")
	for _, line := range lines[1:] {
		assert.NotContains(t, line, "\x1b", "no table row renders with selection styling")
	}

	cmd = s.update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	require.NotNil(t, cmd)
	assert.Equal(t, popMsg{}, cmd(), "esc pops the screen")
}
