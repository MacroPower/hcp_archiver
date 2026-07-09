package progress

import "charm.land/lipgloss/v2"

// Palette for the live view. In lipgloss v2 [lipgloss.Color] is a function
// returning an [image/color.Color], and [lipgloss.Style.Render] always emits
// truecolor; Bubble Tea downsamples the composed view to the terminal's
// profile, so the styles here stay fully saturated and render deterministically.
var (
	// Spinner tint for the liveness indicator.
	styleSpinner = lipgloss.NewStyle().Foreground(lipgloss.Color("#7D56F4"))
	// Phase name, emphasized in bold.
	stylePhase = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7AA2F7"))
	// Current org, project, or workspace target.
	styleTarget = lipgloss.NewStyle().Foreground(lipgloss.Color("#9ECE6A"))
	// Bytes, rate, and elapsed, in a muted tone.
	styleMeta = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A8F98"))
	// Unit-progress fraction beside the bar.
	styleCount = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A8F98"))

	// Done count, in green.
	styleDone = lipgloss.NewStyle().Foreground(lipgloss.Color("#9ECE6A"))
	// Errored count, in red.
	styleErrored = lipgloss.NewStyle().Foreground(lipgloss.Color("#F7768E"))
	// Forbidden count, in amber.
	styleForbidden = lipgloss.NewStyle().Foreground(lipgloss.Color("#E0AF68"))

	// Summary block heading.
	styleSummaryHead = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7AA2F7"))

	// Resumed tag, dimmed so it reads as an aside.
	styleResumed = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#8A8F98"))

	// Filled and empty colors of the bar, shared with the indeterminate marquee
	// so the two read as one component.
	barFullColor  = lipgloss.Color("#7AA2F7")
	barEmptyColor = lipgloss.Color("#3B4261")

	// Moving block and track of the indeterminate marquee.
	styleBarBlock = lipgloss.NewStyle().Foreground(barFullColor)
	styleBarTrack = lipgloss.NewStyle().Foreground(barEmptyColor)
)
