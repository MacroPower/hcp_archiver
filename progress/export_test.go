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
	Phase        string
	Tasks        []PanelTask
	Tally        manifest.Tally
	Elapsed      time.Duration
	PhaseElapsed time.Duration
	Rate         float64
	WireBytes    int64
	RateLimited  int64
	Total        int
	Completed    int
	Workers      int
	MaxWorkers   int
}

// PanelTask mirrors one in-flight work item fed to the panel by a test.
type PanelTask struct {
	Name  string
	Total int
	Done  int
}

// snap converts the test inputs into the internal snapshot.
func (ps PanelSnapshot) snap() snapshot {
	tasks := make([]taskProgress, 0, len(ps.Tasks))
	for _, t := range ps.Tasks {
		tasks = append(tasks, taskProgress{name: t.Name, total: t.Total, done: t.Done})
	}

	return snapshot{
		phase:        ps.Phase,
		tasks:        tasks,
		tally:        ps.Tally,
		elapsed:      ps.Elapsed,
		phaseElapsed: ps.PhaseElapsed,
		rate:         ps.Rate,
		wireBytes:    ps.WireBytes,
		rateLimited:  ps.RateLimited,
		total:        ps.Total,
		completed:    ps.Completed,
		workers:      ps.Workers,
		maxWorkers:   ps.MaxWorkers,
	}
}

// ObserveThroughput feeds each snapshot to a fresh model's throughput window in
// turn and returns the rate derived after the last, exposing the wire-byte
// sampling to tests.
func ObserveThroughput(snaps []PanelSnapshot) float64 {
	m := newTUIModel(nil, nil)
	for i := range snaps {
		m.observe(snaps[i].snap())
	}

	return m.throughput(snaps[len(snaps)-1].snap())
}

// TakeWireBytes exposes the wire-byte figure a snapshot of r carries, so tests
// can assert both the counter path and the committed-bytes fallback.
func TakeWireBytes(r *Reporter) int64 {
	return r.lockedTake().wireBytes
}

// RenderPanel renders the live two-line panel for ps, using a fresh model so the
// spinner shows its first frame.
func RenderPanel(ps PanelSnapshot) string {
	m := newTUIModel(nil, nil)

	return m.render(ps.snap())
}

// RenderPanelAt renders the panel after a window-size message of the given
// dimensions, exercising the bar resize, line clipping, and task-line budget.
func RenderPanelAt(ps PanelSnapshot, width, height int) string {
	m := newTUIModel(nil, nil)
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})

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
