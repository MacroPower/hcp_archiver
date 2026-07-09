package progress

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"

	progressbar "charm.land/bubbles/v2/progress"
	tea "charm.land/bubbletea/v2"

	"go.jacobcolvin.com/hcp_archiver/manifest"
)

// barWidth is the fixed cell width of the determinate progress bar.
const barWidth = 30

// tuiModel is the Bubble Tea model backing the human view on a terminal.
//
// It owns no counters of its own: each render pulls a fresh [snapshot] through
// take, so the panel always mirrors the live ledger. The spinner is both the
// liveness indicator and the repaint clock. Its tick drives re-renders, so the
// bar and counts advance without any separate ticker. Create instances with
// [newTUIModel].
type tuiModel struct {
	bar       progressbar.Model
	take      func() snapshot
	interrupt func()
	spin      spinner.Model
}

// newTUIModel creates a new [tuiModel] that renders snapshots from take and, on
// a quit key, invokes interrupt before quitting. Either function may be nil in
// tests that only exercise rendering. It returns a pointer so Bubble Tea passes
// the heavy model around by reference.
func newTUIModel(take func() snapshot, interrupt func()) *tuiModel {
	spin := spinner.New(spinner.WithSpinner(spinner.Dot))
	spin.Style = styleSpinner

	return &tuiModel{
		spin:      spin,
		bar:       progressbar.New(progressbar.WithWidth(barWidth), progressbar.WithoutPercentage()),
		take:      take,
		interrupt: interrupt,
	}
}

// Init starts the spinner, whose ticks drive every subsequent repaint.
func (m *tuiModel) Init() tea.Cmd {
	return m.spin.Tick
}

// Update advances the spinner on its tick and, on ctrl+c or q, runs the
// interrupt callback before quitting. Raw mode suppresses the kernel's SIGINT,
// so the quit keys are handled here explicitly.
func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.interrupt != nil {
				m.interrupt()
			}

			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		// Shrink the bar on a narrow terminal so the inline panel does not wrap;
		// a fresh model (as the golden tests use) keeps the full barWidth.
		if msg.Width > 0 {
			m.bar.SetWidth(min(barWidth, msg.Width))
		}

		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd

		m.spin, cmd = m.spin.Update(msg)

		return m, cmd
	}

	return m, nil
}

// View renders the current snapshot into an inline (non-alt-screen) view, so log
// lines printed by the sink stay in scrollback above the pinned panel.
func (m *tuiModel) View() tea.View {
	return tea.NewView(m.render(m.take()))
}

// render formats a snapshot as the two-line panel: the spinner, phase, bar or
// spinner-only progress, and target on line one; the colored per-status counts
// and byte/rate/elapsed metadata on line two.
func (m *tuiModel) render(snap snapshot) string {
	var line1 strings.Builder

	line1.WriteString(m.spin.View())

	if snap.phase != "" {
		line1.WriteString(stylePhase.Render(snap.phase))
	}

	// A determinate phase guarantees total > 0, so the percentage is safe.
	if snap.hasBar() {
		percent := float64(snap.completed) / float64(snap.total)

		fmt.Fprintf(&line1, " %s %s", m.bar.ViewAs(percent),
			styleCount.Render(fmt.Sprintf("%d/%d", snap.completed, snap.total)))
	}

	if snap.tally.Target != "" {
		fmt.Fprintf(&line1, " %s", styleTarget.Render(snap.tally.Target))
	}

	t := snap.tally
	line2 := fmt.Sprintf("  %s  %s",
		statusCounts(t),
		styleMeta.Render(fmt.Sprintf("%s  %s/s  %s",
			humanBytes(t.BytesDownloaded),
			humanBytes(int64(snap.rate)),
			snap.elapsed.Round(time.Second),
		)),
	)

	return line1.String() + "\n" + line2
}

// statusCounts renders the styled done/errored/forbidden triple shared by the
// live panel and the summary block.
func statusCounts(t manifest.Tally) string {
	return fmt.Sprintf("%s %s %s",
		styleDone.Render(fmt.Sprintf("done=%d", t.Done)),
		styleErrored.Render(fmt.Sprintf("errored=%d", t.Errored)),
		styleForbidden.Render(fmt.Sprintf("forbidden=%d", t.Forbidden)),
	)
}
