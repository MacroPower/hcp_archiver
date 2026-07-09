package progress

import (
	"time"

	"charm.land/bubbles/v2/spinner"

	tea "charm.land/bubbletea/v2"

	"go.jacobcolvin.com/hcp_archiver/manifest"
)

// SplitLogLines exposes splitLogLines so the external test package can exercise
// the line-splitting the program path relies on.
var SplitLogLines = splitLogLines

// PanelSnapshot are the deterministic inputs a golden test feeds to
// [RenderPanel] and [RenderSummary]. It mirrors the internal snapshot so tests
// stay in the external package without reaching into unexported fields. Whether
// a bar renders is derived from Total, matching production.
type PanelSnapshot struct {
	Phase     string
	Tally     manifest.Tally
	Elapsed   time.Duration
	Rate      float64
	Total     int
	Completed int
}

// snap converts the test inputs into the internal snapshot.
func (ps PanelSnapshot) snap() snapshot {
	return snapshot{
		phase:     ps.Phase,
		tally:     ps.Tally,
		elapsed:   ps.Elapsed,
		rate:      ps.Rate,
		total:     ps.Total,
		completed: ps.Completed,
	}
}

// RenderPanel renders the live two-line panel for ps, using a fresh model so the
// spinner shows its first frame.
func RenderPanel(ps PanelSnapshot) string {
	m := newTUIModel(nil, nil)

	return m.render(ps.snap())
}

// RenderPanelAt renders the panel after a window-size message of the given
// width, exercising the bar resize and line clipping.
func RenderPanelAt(ps PanelSnapshot, width int) string {
	m := newTUIModel(nil, nil)
	m.Update(tea.WindowSizeMsg{Width: width, Height: 24})

	return m.render(ps.snap())
}

// MarqueeTick advances a fresh model's spinner tick n times and renders ps,
// exposing the indeterminate marquee's animation to tests.
func MarqueeTick(ps PanelSnapshot, n int) string {
	m := newTUIModel(nil, nil)
	for range n {
		m.Update(spinner.TickMsg{})
	}

	return m.render(ps.snap())
}

// RenderSummary renders the styled summary block for ps.
func RenderSummary(ps PanelSnapshot) string {
	var r Reporter

	return r.summaryBlock(ps.snap())
}
