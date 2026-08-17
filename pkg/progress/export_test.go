package progress

import (
	"time"

	"charm.land/bubbles/v2/spinner"

	tea "charm.land/bubbletea/v2"

	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
)

// SplitLogLines exposes splitLogLines so the external test package can exercise
// the line-splitting the queue path relies on.
var SplitLogLines = splitLogLines

// MaxQueuedLogLines exposes the sink's queue bound so the overflow tests track
// the production cap.
const MaxQueuedLogLines = maxQueuedLogLines

// PanelSnapshot are the deterministic inputs a golden test feeds to
// [RenderPanel] and [RenderSummary]. It mirrors the internal snapshot so tests
// stay in the external package without reaching into unexported fields. Whether
// a bar renders is derived from Total, matching production.
type PanelSnapshot struct {
	Phase           string
	Tasks           []PanelTask
	Tally           manifest.Tally
	Elapsed         time.Duration
	PhaseElapsed    time.Duration
	Rate            float64
	UploadRate      float64
	RPS             float64
	PausedFor       time.Duration
	WireBytes       int64
	UploadWireBytes int64
	RateLimited     int64
	Remote          RemoteStats
	Total           int
	Completed       int
	HasRate         bool
	HasRemote       bool
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
		phase:           ps.Phase,
		tasks:           tasks,
		tally:           ps.Tally,
		elapsed:         ps.Elapsed,
		phaseElapsed:    ps.PhaseElapsed,
		rate:            ps.Rate,
		uploadRate:      ps.UploadRate,
		rps:             ps.RPS,
		pausedFor:       ps.PausedFor,
		wireBytes:       ps.WireBytes,
		uploadWireBytes: ps.UploadWireBytes,
		rateLimited:     ps.RateLimited,
		remote:          ps.Remote,
		total:           ps.Total,
		completed:       ps.Completed,
		hasRate:         ps.HasRate,
		hasRemote:       ps.HasRemote,
	}
}

// ObserveThroughput feeds each snapshot to a fresh model's throughput window in
// turn and returns the rate derived after the last, exposing the wire-byte
// sampling to tests.
func ObserveThroughput(snaps []PanelSnapshot) float64 {
	m := newTUIModel(nil, nil, nil)
	for i := range snaps {
		m.observe(snaps[i].snap())
	}

	return m.throughput(snaps[len(snaps)-1].snap())
}

// ObserveUploadThroughput feeds each snapshot to a fresh model's upload
// throughput window in turn and returns the rate derived after the last,
// exposing the upload wire-byte sampling to tests.
func ObserveUploadThroughput(snaps []PanelSnapshot) float64 {
	m := newTUIModel(nil, nil, nil)
	for i := range snaps {
		m.observe(snaps[i].snap())
	}

	return m.uploadThroughput(snaps[len(snaps)-1].snap())
}

// TakeWireBytes exposes the wire-byte figure a snapshot of r carries, so tests
// can assert both the counter path and the committed-bytes fallback.
func TakeWireBytes(r *Reporter) int64 {
	return r.lockedTake().wireBytes
}

// TakeUploadWireBytes exposes the upload wire-byte figure a snapshot of r
// carries, so tests can assert both the counter path and the committed
// remote-bytes fallback.
func TakeUploadWireBytes(r *Reporter) int64 {
	return r.lockedTake().uploadWireBytes
}

// RenderPanel renders the live panel for ps, using a fresh model so the
// spinner shows its first frame.
func RenderPanel(ps PanelSnapshot) string {
	m := newTUIModel(nil, nil, nil)

	return m.render(ps.snap())
}

// RenderPanelAt renders the panel after a window-size message of the given
// dimensions, exercising the bar resize, line clipping, and task-line budget.
func RenderPanelAt(ps PanelSnapshot, width, height int) string {
	m := newTUIModel(nil, nil, nil)
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})

	return m.render(ps.snap())
}

// MarqueeTick advances a fresh model's spinner tick n times and renders ps,
// exposing the indeterminate marquee's animation to tests.
func MarqueeTick(ps PanelSnapshot, n int) string {
	m := newTUIModel(nil, nil, nil)
	for range n {
		m.Update(spinner.TickMsg{})
	}

	return m.render(ps.snap())
}

// newModelAt builds a model taking snapshots from take, sized by an initial
// window message when width or height is set (a zero size keeps the unclipped
// task budget, as the golden tests use).
func newModelAt(width, height int, take func() snapshot) *tuiModel {
	m := newTUIModel(take, nil, nil)
	if width > 0 || height > 0 {
		m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	}

	return m
}

// RenderPanelFrames drives one persistent model through snaps by ticks at the
// given terminal size, returning each rendered frame. The model ratchets its
// held frame height on each render, so the panel holds its height across
// frames as it does in a live run.
func RenderPanelFrames(width, height int, snaps ...PanelSnapshot) []string {
	idx := 0
	m := newModelAt(width, height, func() snapshot { return snaps[idx].snap() })

	frames := make([]string, len(snaps))

	for idx = range snaps {
		m.Update(spinner.TickMsg{})

		frames[idx] = m.render(m.snap)
	}

	return frames
}

// RenderPanelResize grows a persistent model's frame to its peak at the first
// size, then renders after resizing to the second, returning the post-resize
// frame. It exercises the held height's clamp: a terminal too short for the
// peak pulls the frame back down so the panel still fits.
func RenderPanelResize(width1, height1, width2, height2 int, peak, after PanelSnapshot) string {
	snaps := []PanelSnapshot{peak, after}
	idx := 0
	m := newModelAt(width1, height1, func() snapshot { return snaps[idx].snap() })

	m.Update(spinner.TickMsg{})
	m.render(m.snap)

	idx = 1

	m.Update(tea.WindowSizeMsg{Width: width2, Height: height2})
	m.Update(spinner.TickMsg{})

	return m.render(m.snap)
}

// RenderSummary renders the styled summary block for ps.
func RenderSummary(ps PanelSnapshot) string {
	var r Reporter

	return r.summaryBlock(ps.snap())
}
