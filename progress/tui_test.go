package progress_test

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/stretchr/testify/assert"

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
			BytesDownloaded: 3*1024*1024 + 512*1024,
		},
		Elapsed:   90 * time.Second,
		Rate:      128 * 1024,
		Total:     20,
		Completed: 7,
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

// line2 renders a panel and returns its stripped, undecorated second line.
func line2(panel progress.PanelSnapshot) string {
	parts := strings.SplitN(progress.RenderPanel(panel), "\n", 2)

	return ansi.Strip(parts[len(parts)-1])
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
	assert.Equal(t, strings.Index(b, "errored="), strings.Index(a, "errored="),
		"errored column holds as done rolls over")
	assert.Equal(t, indexOfMeta(b), indexOfMeta(a), "metadata column holds")
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

// indexOfMeta returns the column of the metadata's byte figure on a stripped
// line-two string, the first field after the per-status counts.
func indexOfMeta(line string) int {
	return strings.Index(line, "MiB")
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
			BytesDownloaded:   5 * 1024 * 1024,
		},
		Elapsed: 200 * time.Second,
	}

	golden.RequireEqual(t, []byte(progress.RenderSummary(panel)))
}
