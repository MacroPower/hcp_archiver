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
	panel.Workers = 4
	panel.MaxWorkers = 16

	lines := strings.Split(progress.RenderPanel(panel), "\n")
	require.Len(t, lines, 3)

	counts := ansi.Strip(lines[1])
	meta := ansi.Strip(lines[2])

	pairs := map[string]string{"✓": "⇣", "✗": "⇢", "⊘": "◷", "↻": "⚙"}
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
			Done:              100,
			AbsentPermanently: 3,
			Skipped:           2,
			Errored:           4,
			Forbidden:         6,
			NotApplicable:     1,
			Retried:           5,
			BytesDownloaded:   5 * 1024 * 1024,
		},
		Elapsed: 200 * time.Second,
	}

	golden.RequireEqual(t, []byte(progress.RenderSummary(panel)))
}

func TestRenderPanel_Workers(t *testing.T) {
	t.Parallel()

	// The adaptive worker readout rides in the metadata segment, and once any
	// rate limiting has been observed the amber 429 total follows it, so a
	// shrunken pool carries its own explanation.
	panel := barPanel()
	panel.Workers = 4
	panel.MaxWorkers = 16
	panel.RateLimited = 12

	golden.RequireEqual(t, []byte(progress.RenderPanel(panel)))
}

func TestRenderPanel_WorkersCleanRunOmits429s(t *testing.T) {
	t.Parallel()

	panel := barPanel()
	panel.Workers = 16
	panel.MaxWorkers = 16

	out := ansi.Strip(progress.RenderPanel(panel))
	assert.Contains(t, out, "16/16 workers")
	assert.NotContains(t, out, "429", "a run never rate limited shows no 429 readout")
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
