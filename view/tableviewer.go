package view

import (
	"slices"

	"charm.land/bubbles/v2/table"
	"go.jacobcolvin.com/niceyaml/bubbles/yamlviewport"

	tea "charm.land/bubbletea/v2"
)

// tableMinHeight is the shortest frame a bubbles table can render: its View is
// always the header line joined to its inner viewport, which emits at least
// one line even at zero height.
const tableMinHeight = 2

// tableViewerScreen renders a static table over one archived JSON document
// scrolling through the niceyaml viewport. The table is display-only: keys
// act on the viewport or the screen, never the table, and when the screen is
// too short to hold both, the table yields first — each of its values also
// appears in the document below.
//
// Create instances with [newTableViewerScreen].
type tableViewerScreen struct {
	name        string
	table       table.Model
	vp          yamlviewport.Model
	tableHidden bool
}

// newTableViewerScreen creates a new [tableViewerScreen] named name (its
// breadcrumb segment) with rows laid out under cols, over the JSON content.
// The last column's width is recomputed from the screen width on every
// resize; the widths of the columns before it are kept as given.
func newTableViewerScreen(name string, cols []table.Column, rows []table.Row, content string) *tableViewerScreen {
	return &tableViewerScreen{
		name:  name,
		table: newThemedTable(cols, rows),
		vp:    newThemedYAMLViewport(name, content),
	}
}

// update handles navigation keys itself and forwards scrolling to the
// viewport. The table never sees a message: it is display-only.
func (s *tableViewerScreen) update(msg tea.Msg) tea.Cmd {
	if cmd, handled := scrollKey(msg, func() { s.vp.GotoTop() }, func() { s.vp.GotoBottom() }); handled {
		return cmd
	}

	var cmd tea.Cmd

	s.vp, cmd = s.vp.Update(msg)

	return cmd
}

// view renders the table over the viewport over a footer carrying the scroll
// position and the key hints. The table segment drops out entirely when the
// height budget hides it.
func (s *tableViewerScreen) view() string {
	body := s.vp.View() + "\n" + viewerFooter(s.vp.ScrollPercent(), "w wrap")

	if s.tableHidden {
		return body
	}

	return s.table.View() + "\n" + body
}

// crumb names the screen's breadcrumb segment.
func (s *tableViewerScreen) crumb() string { return s.name }

// setSize splits the screen body between the table and the viewport, minus
// the footer line. The table gets its natural height (header plus rows) and
// no more, so it sits flush against the document; when the body is too short
// for both, the viewport keeps at least one line and the table is clipped and
// then hidden — never sized below its two-line rendering floor.
func (s *tableViewerScreen) setSize(width, height int) {
	avail := max(height-1, 0)

	s.tableHidden = avail <= tableMinHeight
	if s.tableHidden {
		s.vp.SetWidth(width)
		s.vp.SetHeight(avail)

		return
	}

	tableHeight := max(min(1+len(s.table.Rows()), avail-1), tableMinHeight)

	// Column widths live in the columns, not the table: SetWidth only sizes
	// the inner viewport, so the flex column is pushed via SetColumns. A width
	// of zero or less would drop the column entirely, hence the floor at 1.
	cols := slices.Clone(s.table.Columns())

	fixed := 0
	for _, c := range cols[:len(cols)-1] {
		fixed += c.Width
	}

	cols[len(cols)-1].Width = max(width-fixed, 1)

	s.table.SetColumns(cols)
	s.table.SetWidth(width)
	s.table.SetHeight(tableHeight)

	s.vp.SetWidth(width)
	s.vp.SetHeight(avail - tableHeight)
}
