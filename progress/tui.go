package progress

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	progressbar "charm.land/bubbles/v2/progress"
	tea "charm.land/bubbletea/v2"

	"go.jacobcolvin.com/hcp_archiver/manifest"
)

// Fixed column widths that keep the panel from reflowing as values change. Every
// field before the trailing target (line one) or metadata (line two) occupies a
// constant width, so a growing count or an appearing bar never shifts what
// follows it. The bar and percent hold their columns whether the phase is
// determinate or not, so the target stays put across phase transitions.
const (
	barWidth   = 30 // determinate bar and indeterminate marquee track.
	phaseWidth = 10 // widest phase name ("workspaces").
	pctWidth   = 4  // "100%" down to "  0%".

	marqueeBlock = 5 // moving block width within the indeterminate track.

	// Digit reserves for the per-status counts; the numbers left-align into
	// invisible trailing pad, so the next column never moves until a count
	// exceeds its reserve (rare, and then only once as it gains a digit). Done
	// spans the whole org, so it reserves the most.
	countDoneWidth      = 6
	countErroredWidth   = 4
	countForbiddenWidth = 4
)

// Bar cell glyphs, matching the determinate bar's defaults so the marquee looks
// like the same component.
const (
	barFullChar  = "▌"
	barEmptyChar = "░"
)

// tuiModel is the Bubble Tea model backing the human view on a terminal.
//
// It owns no counters of its own: each render pulls a fresh [snapshot] through
// take, so the panel always mirrors the live ledger. The spinner is both the
// liveness indicator and the repaint clock. Its tick drives re-renders and
// advances the indeterminate marquee, so the bar and counts move without any
// separate ticker. Create instances with [newTUIModel].
type tuiModel struct {
	take      func() snapshot
	interrupt func()
	bar       progressbar.Model
	spin      spinner.Model
	width     int
	tick      int
}

// newTUIModel creates a new [tuiModel] that renders snapshots from take and, on
// a quit key, invokes interrupt before quitting. Either function may be nil in
// tests that only exercise rendering. It returns a pointer so Bubble Tea passes
// the heavy model around by reference.
func newTUIModel(take func() snapshot, interrupt func()) *tuiModel {
	spin := spinner.New(spinner.WithSpinner(spinner.Dot))
	spin.Style = styleSpinner

	bar := progressbar.New(progressbar.WithWidth(barWidth), progressbar.WithoutPercentage())
	bar.FullColor = barFullColor
	bar.EmptyColor = barEmptyColor

	return &tuiModel{
		spin:      spin,
		bar:       bar,
		take:      take,
		interrupt: interrupt,
	}
}

// Init starts the spinner, whose ticks drive every subsequent repaint.
func (m *tuiModel) Init() tea.Cmd {
	return m.spin.Tick
}

// Update advances the spinner and marquee on each tick, records the terminal
// width, and, on ctrl+c or q, runs the interrupt callback before quitting. Raw
// mode suppresses the kernel's SIGINT, so the quit keys are handled here.
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
		// Record the width to clip each line, and shrink the bar so the panel
		// never wraps to a third line. A fresh model (as the golden tests use)
		// keeps the full barWidth and does not clip.
		m.width = msg.Width
		if msg.Width > 0 {
			m.bar.SetWidth(min(barWidth, msg.Width))
		}

		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd

		m.tick++
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

// render formats a snapshot as the two-line panel. Line one carries the spinner,
// a fixed-width phase, the bar or its indeterminate marquee, the percent, and the
// target; line two carries the colored per-status counts and the byte, rate, and
// elapsed metadata. Every field before the trailing target and metadata holds a
// constant width, so the panel does not reflow as values change.
func (m *tuiModel) render(snap snapshot) string {
	var line1 strings.Builder

	line1.WriteString(m.spin.View())
	line1.WriteString(stylePhase.Render(padRight(snap.phase, phaseWidth)))
	line1.WriteByte(' ')

	if snap.hasBar() {
		// Clamp to [0,1] so a miscounted completed/total can never widen the
		// percent past its 4-cell reserve (or read negative), keeping the bar and
		// its label in lockstep. Truncating toward zero floors the percent, so it
		// never reads 100% before the last unit lands.
		percent := min(max(float64(snap.completed)/float64(snap.total), 0), 1)
		line1.WriteString(m.bar.ViewAs(percent))
		fmt.Fprintf(&line1, " %s", styleCount.Render(fmt.Sprintf("%3d%%", int(percent*100))))
	} else {
		line1.WriteString(marquee(m.bar.Width(), m.tick))
		fmt.Fprintf(&line1, " %s", strings.Repeat(" ", pctWidth))
	}

	if snap.tally.Target != "" {
		fmt.Fprintf(&line1, " %s", styleTarget.Render(snap.tally.Target))
	}

	// The resumed tag rides at the end of the line so nothing follows it, and it
	// is fixed for the whole run, so it never causes a mid-run reflow.
	if snap.tally.Resumed {
		fmt.Fprintf(&line1, "  %s", styleResumed.Render("(resumed)"))
	}

	t := snap.tally
	line2 := fmt.Sprintf("  %s  %s",
		statusCounts(t, countDoneWidth, countErroredWidth, countForbiddenWidth),
		styleMeta.Render(fmt.Sprintf("%s  %s/s  %s",
			humanBytes(t.BytesDownloaded),
			humanBytes(int64(snap.rate)),
			snap.elapsed.Round(time.Second),
		)),
	)

	return m.fit(line1.String()) + "\n" + m.fit(line2)
}

// fit clips a rendered line to the terminal width so the panel never wraps onto
// an extra line. A zero width (before the first size message) leaves it whole.
func (m *tuiModel) fit(line string) string {
	if m.width <= 0 {
		return line
	}

	return ansi.Truncate(line, m.width, "")
}

// statusCounts renders the styled done/errored/forbidden triple shared by the
// live panel and the summary block. The width arguments left-pad each count so
// the live panel's columns stay put as values grow; pass zero widths for the
// summary's tight spacing.
func statusCounts(t manifest.Tally, doneWidth, erroredWidth, forbiddenWidth int) string {
	return fmt.Sprintf("%s %s %s",
		styleDone.Render(fmt.Sprintf("done=%-*d", doneWidth, t.Done)),
		styleErrored.Render(fmt.Sprintf("errored=%-*d", erroredWidth, t.Errored)),
		styleForbidden.Render(fmt.Sprintf("forbidden=%-*d", forbiddenWidth, t.Forbidden)),
	)
}

// marquee renders an indeterminate bar of width cells: a block that ping-pongs
// across the track, its position derived from tick so a fresh model (tick zero)
// renders it deterministically at the left edge.
func marquee(width, tick int) string {
	if width <= 0 {
		return ""
	}

	block := min(marqueeBlock, width)

	start := 0
	if span := width - block; span > 0 {
		phase := tick % (2 * span)
		if phase <= span {
			start = phase
		} else {
			start = 2*span - phase
		}
	}

	var b strings.Builder

	writeRun(&b, styleBarTrack, barEmptyChar, start)
	writeRun(&b, styleBarBlock, barFullChar, block)
	writeRun(&b, styleBarTrack, barEmptyChar, width-start-block)

	return b.String()
}

// writeRun writes n copies of char styled by style, or nothing when n is zero so
// no empty escape sequence is emitted.
func writeRun(b *strings.Builder, style lipgloss.Style, char string, n int) {
	if n <= 0 {
		return
	}

	b.WriteString(style.Render(strings.Repeat(char, n)))
}

// padRight returns s padded with spaces to width, or truncated to width. Phase
// names are ASCII, so byte length equals cell width.
func padRight(s string, width int) string {
	if len(s) >= width {
		return s[:width]
	}

	return s + strings.Repeat(" ", width-len(s))
}
