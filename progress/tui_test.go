package progress_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/progress"
)

// barPanel is a deterministic determinate-phase snapshot: a fresh model renders
// the spinner's first frame and a static bar, so the panel is stable across
// runs.
func barPanel() progress.PanelSnapshot {
	return progress.PanelSnapshot{
		Phase: "workspaces",
		Tally: manifest.Tally{
			Target:          "acme/prod",
			Done:            42,
			Errored:         2,
			Forbidden:       1,
			Retried:         3,
			BytesDownloaded: 3*1024*1024 + 512*1024,
		},
		Elapsed:      90 * time.Second,
		PhaseElapsed: 63 * time.Second,
		Rate:         128 * 1024,
		Total:        20,
		Completed:    7,
	}
}

func TestRenderPanel_Bar(t *testing.T) {
	t.Parallel()

	golden.RequireEqual(t, []byte(progress.RenderPanel(barPanel())))
}

func TestRenderPanel_Indeterminate(t *testing.T) {
	t.Parallel()

	// An indeterminate phase swaps the bar for a marquee and blanks the percent,
	// holding the same columns so the target does not move.
	golden.RequireEqual(t, []byte(progress.RenderPanel(indeterminatePanel())))
}

func TestRenderPanel_Tasks(t *testing.T) {
	t.Parallel()

	// In-flight work items each get a line between the phase bar and the
	// counts: their own bar, percent, unit fraction, and name, in registration
	// order. The target is empty during the workspaces phase, since the task
	// lines carry the names, so line one ends at its eta column.
	panel := barPanel()
	panel.Tally.Target = ""
	panel.Tasks = []progress.PanelTask{
		{Name: "acme/big-workspace", Total: 5001, Done: 600},
		{Name: "acme/mid-workspace", Total: 60, Done: 31},
		{Name: "acme/tiny", Total: 4, Done: 3},
	}

	golden.RequireEqual(t, []byte(progress.RenderPanel(panel)))
}

func TestRenderPanel_TaskOverflow(t *testing.T) {
	t.Parallel()

	// A pool larger than the cap lists the first eight items and counts the
	// rest on an overflow line.
	panel := barPanel()
	panel.Tally.Target = ""

	for i := range 11 {
		panel.Tasks = append(panel.Tasks, progress.PanelTask{
			Name:  fmt.Sprintf("acme/ws-%02d", i),
			Total: 10 + i,
			Done:  i,
		})
	}

	golden.RequireEqual(t, []byte(progress.RenderPanel(panel)))
}

func TestRenderPanel_TaskAlignsWithPhaseBar(t *testing.T) {
	t.Parallel()

	// Every task line's bar starts at the same column as the phase bar, so the
	// panel reads as one grid.
	panel := barPanel()
	panel.Tasks = []progress.PanelTask{
		{Name: "ws", Total: 10, Done: 5},
		{Name: "ws2", Total: 8, Done: 1},
	}

	rendered := strings.Split(progress.RenderPanel(panel), "\n")
	require.Len(t, rendered, 5)

	for _, line := range rendered[1:3] {
		assert.Equal(t, barColumn(t, rendered[0]), barColumn(t, line),
			"task bar column matches phase bar column")
	}
}

func TestRenderPanel_TaskLinesFitTerminalHeight(t *testing.T) {
	t.Parallel()

	// A short terminal tightens the task budget so the whole panel fits: at
	// six rows there is room for two task lines beside the first line, the
	// overflow line, the counts, and the metadata.
	panel := barPanel()
	for i := range 11 {
		panel.Tasks = append(panel.Tasks, progress.PanelTask{
			Name:  fmt.Sprintf("acme/ws-%02d", i),
			Total: 10,
			Done:  i,
		})
	}

	rendered := progress.RenderPanelAt(panel, 80, 6)
	lines := strings.Split(rendered, "\n")

	assert.Len(t, lines, 6, "panel height matches the terminal")
	assert.Contains(t, ansi.Strip(rendered), "+9 more active")
}

func TestRenderPanel_HeightHoldsAsTasksFinish(t *testing.T) {
	t.Parallel()

	// The panel must not shrink as work items finish; a shrinking frame corrupts
	// it mid-log (see render). The height holds and the surviving task still shows.
	frames := progress.RenderPanelFrames(0, 0, tasksPanel(5), tasksPanel(2))

	assert.Equal(t, strings.Count(frames[0], "\n"), strings.Count(frames[1], "\n"),
		"panel height holds as tasks finish")
	assert.Contains(t, ansi.Strip(frames[1]), "acme/ws-00", "remaining task still shown")
}

func TestRenderPanel_HighWaterClampsOnResizeDown(t *testing.T) {
	t.Parallel()

	// The reserved region is capped at what the terminal fits, so a resize to a
	// shorter terminal pulls the high-water back down and the panel still fits
	// without truncating a live task row.
	//
	// Grow to peak on a tall terminal, then shrink to six rows.
	frame := progress.RenderPanelResize(80, 20, 80, 6, tasksPanel(8), tasksPanel(1))
	lines := strings.Split(frame, "\n")

	assert.LessOrEqual(t, len(lines), 6, "panel fits the shorter terminal")
	assert.Contains(t, ansi.Strip(frame), "acme/ws-00", "live task not truncated by padding")
}

// barColumn returns the display column at which a rendered line's bar begins.
func barColumn(t *testing.T, line string) int {
	t.Helper()

	stripped := ansi.Strip(line)
	i := strings.IndexRune(stripped, '▌')
	require.GreaterOrEqual(t, i, 0, "line carries a bar")

	return ansi.StringWidth(stripped[:i])
}

func TestRenderPanel_Resumed(t *testing.T) {
	t.Parallel()

	// A resumed run tags the end of line one; the cumulative counts already
	// reflect prior work, so nothing else about the panel changes.
	panel := barPanel()
	panel.Tally.Resumed = true

	golden.RequireEqual(t, []byte(progress.RenderPanel(panel)))
}

// tasksPanel is barPanel with the target cleared and n in-flight work items,
// shared by the task-region tests.
func tasksPanel(n int) progress.PanelSnapshot {
	p := barPanel()
	p.Tally.Target = ""

	for i := range n {
		p.Tasks = append(p.Tasks, progress.PanelTask{
			Name:  fmt.Sprintf("acme/ws-%02d", i),
			Total: 10,
			Done:  i,
		})
	}

	return p
}

// indeterminatePanel is barPanel with the phase's total unknown, so the bar
// becomes a marquee and the percent goes blank.
func indeterminatePanel() progress.PanelSnapshot {
	panel := barPanel()
	panel.Phase = "registry"
	panel.Total = -1
	panel.Completed = 0

	return panel
}

// line1 renders a panel and returns its stripped, undecorated first line.
func line1(panel progress.PanelSnapshot) string {
	return ansi.Strip(strings.SplitN(progress.RenderPanel(panel), "\n", 2)[0])
}

// line2 renders a panel and returns its stripped, undecorated second line, the
// status counts on a task-less panel.
func line2(panel progress.PanelSnapshot) string {
	parts := strings.Split(progress.RenderPanel(panel), "\n")

	return ansi.Strip(parts[1])
}

func TestRenderPanel_CountRolloverIsStable(t *testing.T) {
	t.Parallel()

	// A count growing a digit (99 -> 100) must not shift what follows it: the
	// trailing metadata stays at the same column.
	before := barPanel()
	before.Tally.Done = 99

	after := barPanel()
	after.Tally.Done = 100

	b := line2(before)
	a := line2(after)

	assert.Equal(t, ansi.StringWidth(b), ansi.StringWidth(a), "line width holds")
	assert.Equal(t, strings.Index(b, "errored"), strings.Index(a, "errored"),
		"errored column holds as done rolls over")
	assert.Equal(t, strings.Index(b, "retried"), strings.Index(a, "retried"),
		"retried column holds as done rolls over")
}

func TestRenderPanel_MetaAlignsWithCounts(t *testing.T) {
	t.Parallel()

	// Each metadata glyph sits in the display column of the status glyph above
	// it, so the panel's two closing lines read as one grid.
	panel := barPanel()
	panel.HasRate = true
	panel.RPS = 12

	lines := strings.Split(progress.RenderPanel(panel), "\n")
	require.Len(t, lines, 3)

	counts := ansi.Strip(lines[1])
	meta := ansi.Strip(lines[2])

	pairs := map[string]string{"✓": "⇣", "✗": "⇢", "⊘": "◷", "↻": "⇉"}
	for status, readout := range pairs {
		assert.Equal(t, column(t, counts, status), column(t, meta, readout),
			"%s sits under %s", readout, status)
	}
}

// column returns the display column at which line's first occurrence of sub
// begins.
func column(t *testing.T, line, sub string) int {
	t.Helper()

	i := strings.Index(line, sub)
	require.GreaterOrEqual(t, i, 0, "line carries %q", sub)

	return ansi.StringWidth(line[:i])
}

func TestRenderPanel_OvercompleteIsStable(t *testing.T) {
	t.Parallel()

	// A miscounted completed past total must not widen the percent past its
	// reserve: it clamps to 100% and holds the target column.
	full := barPanel()
	full.Completed = full.Total

	over := barPanel()
	over.Completed = full.Total * 50

	f := line1(full)
	o := line1(over)

	assert.Equal(t, ansi.StringWidth(f), ansi.StringWidth(o), "line width holds when overcomplete")
	assert.Equal(t, strings.Index(f, "acme/prod"), strings.Index(o, "acme/prod"),
		"target column holds when overcomplete")
	assert.Contains(t, o, "100%", "percent clamps to 100%")
}

func TestRenderPanel_BarToggleIsStable(t *testing.T) {
	t.Parallel()

	// The bar appearing or disappearing between a determinate and an
	// indeterminate phase must not shift the target column.
	determinate := barPanel()
	indeterminate := indeterminatePanel()
	indeterminate.Tally.Target = determinate.Tally.Target

	d := line1(determinate)
	i := line1(indeterminate)

	assert.Equal(t, ansi.StringWidth(d), ansi.StringWidth(i), "line width holds")
	assert.Equal(t, strings.Index(d, "acme/prod"), strings.Index(i, "acme/prod"),
		"target column holds whether or not the bar is shown")
}

func TestRenderSummary(t *testing.T) {
	t.Parallel()

	panel := progress.PanelSnapshot{
		Tally: manifest.Tally{
			Done:            100,
			Absent:          3,
			Skipped:         2,
			Errored:         4,
			Forbidden:       6,
			NotApplicable:   1,
			Retried:         5,
			BytesDownloaded: 5 * 1024 * 1024,
		},
		Elapsed: 200 * time.Second,
	}

	golden.RequireEqual(t, []byte(progress.RenderSummary(panel)))
}

func TestRenderPanel_RateStatus(t *testing.T) {
	t.Parallel()

	// The adaptive request-rate readout rides in the metadata segment; during
	// a cooldown the amber paused readout follows it, and once any rate
	// limiting has been observed the amber 429 total rides along too, so a
	// slowed rate carries its own explanation.
	panel := barPanel()
	panel.HasRate = true
	panel.RPS = 12
	panel.PausedFor = 4 * time.Second
	panel.RateLimited = 12

	golden.RequireEqual(t, []byte(progress.RenderPanel(panel)))
}

func TestRenderPanel_RateStatusCleanRunOmitsAmber(t *testing.T) {
	t.Parallel()

	panel := barPanel()
	panel.HasRate = true
	panel.RPS = 30

	out := ansi.Strip(progress.RenderPanel(panel))
	assert.Contains(t, out, "30/s")
	assert.NotContains(t, out, "429", "a run never rate limited shows no 429 readout")
	assert.NotContains(t, out, "paused", "no cooldown means no paused readout")
}

func TestRenderSummary_RateLimited(t *testing.T) {
	t.Parallel()

	panel := progress.PanelSnapshot{
		Tally: manifest.Tally{
			Done:            100,
			BytesDownloaded: 5 * 1024 * 1024,
		},
		Elapsed:     200 * time.Second,
		RateLimited: 42,
	}

	golden.RequireEqual(t, []byte(progress.RenderSummary(panel)))
}

// remotePanel is barPanel with a remote configured and a clean transfer tally,
// shared by the remote-readout tests. The upload rate is non-zero so the
// goldens show the panel's live rate segment with a real figure.
func remotePanel() progress.PanelSnapshot {
	panel := barPanel()
	panel.HasRemote = true
	panel.UploadRate = 3.4 * 1024 * 1024 // 3.4 MiB/s
	panel.Remote = progress.RemoteStats{
		UploadedBytes: 1288490188, // 1.2 GiB
		Uploaded:      5432,
		Evicted:       87,
	}

	return panel
}

func TestRenderPanel_Remote(t *testing.T) {
	t.Parallel()

	// A remote-configured run closes the panel with the transfer readout; with
	// nothing failed the whole line stays muted, no amber segment.
	golden.RequireEqual(t, []byte(progress.RenderPanel(remotePanel())))
}

func TestRenderPanel_RemoteFailed(t *testing.T) {
	t.Parallel()

	// Once any remote motion has failed the amber failed segment rides at the
	// end of the readout, the same convention as the 429 total.
	panel := remotePanel()
	panel.Remote.Failed = 3

	golden.RequireEqual(t, []byte(progress.RenderPanel(panel)))
}

func TestRenderPanel_NoRemoteOmitsLine(t *testing.T) {
	t.Parallel()

	// A local-only run carries no remote line at all: presence is gated by the
	// option, not the sampled values.
	out := ansi.Strip(progress.RenderPanel(barPanel()))

	assert.NotContains(t, out, "☁")
	assert.NotContains(t, out, "uploaded")
}

func TestRenderSummary_Remote(t *testing.T) {
	t.Parallel()

	panel := progress.PanelSnapshot{
		Tally: manifest.Tally{
			Done:            100,
			BytesDownloaded: 5 * 1024 * 1024,
		},
		Elapsed:   200 * time.Second,
		HasRemote: true,
		Remote: progress.RemoteStats{
			UploadedBytes: 1288490188,
			Uploaded:      5432,
			Evicted:       87,
			Failed:        3,
		},
	}

	golden.RequireEqual(t, []byte(progress.RenderSummary(panel)))
}

func TestRenderPanel_RemoteTaskLinesFitTerminalHeight(t *testing.T) {
	t.Parallel()

	// The remote readout claims a footer line, so a short terminal reserves
	// five lines rather than four: at six rows one task line fits beside the
	// first line, the overflow line, the counts, the metadata, and the readout.
	panel := remotePanel()
	for i := range 11 {
		panel.Tasks = append(panel.Tasks, progress.PanelTask{
			Name:  fmt.Sprintf("acme/ws-%02d", i),
			Total: 10,
			Done:  i,
		})
	}

	rendered := progress.RenderPanelAt(panel, 80, 6)
	lines := strings.Split(rendered, "\n")

	assert.Len(t, lines, 6, "panel height matches the terminal")
	assert.Contains(t, ansi.Strip(rendered), "+10 more active")
	assert.Contains(t, ansi.Strip(rendered), "☁")
}

func TestRenderPanel_RemoteHeightHoldsOnShortTerminal(t *testing.T) {
	t.Parallel()

	// The high-water pad must respect the tighter remote budget on both of its
	// call sites: as tasks finish on a short terminal the padded region holds
	// the panel's height without ever outgrowing the screen.
	peak := remotePanel()
	peak.Tally.Target = ""

	for i := range 11 {
		peak.Tasks = append(peak.Tasks, progress.PanelTask{
			Name:  fmt.Sprintf("acme/ws-%02d", i),
			Total: 10,
			Done:  i,
		})
	}

	after := remotePanel()
	after.Tally.Target = ""
	after.Tasks = peak.Tasks[:1]

	frames := progress.RenderPanelFrames(80, 6, peak, after)

	for i, frame := range frames {
		assert.LessOrEqual(t, strings.Count(frame, "\n")+1, 6,
			"frame %d fits the terminal", i)
	}

	assert.Equal(t, strings.Count(frames[0], "\n"), strings.Count(frames[1], "\n"),
		"panel height holds as tasks finish")
	assert.Contains(t, ansi.Strip(frames[1]), "acme/ws-00", "remaining task still shown")
}
