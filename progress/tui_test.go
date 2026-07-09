package progress_test

import (
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/golden"

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

func TestRenderPanel_Spinner(t *testing.T) {
	t.Parallel()

	// An indeterminate phase drops the bar and its fraction, leaving the
	// spinner and phase name.
	panel := barPanel()
	panel.Phase = "orgscope"
	panel.Tally.Target = ""
	panel.Total = -1
	panel.Completed = 0

	golden.RequireEqual(t, []byte(progress.RenderPanel(panel)))
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
