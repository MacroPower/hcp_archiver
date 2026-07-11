package progress

import "charm.land/lipgloss/v2"

// Status glyphs shared by the live panel and the summary block, one per colored
// count so a status reads at a glance without parsing labels.
const (
	glyphDone      = "✓"
	glyphErrored   = "✗"
	glyphForbidden = "⊘"
	glyphRetried   = "↻"
)

// Metadata glyphs leading the panel's closing readouts, mirroring the status
// glyphs above them so the panel's last two lines read as one grid.
const (
	glyphBytes   = "⇣"
	glyphRate    = "⇢"
	glyphElapsed = "◷"
	glyphWorkers = "⚙"
)

// Palette for the live view. In lipgloss v2 [lipgloss.Color] is a function
// returning an [image/color.Color], and [lipgloss.Style.Render] always emits
// truecolor; Bubble Tea downsamples the composed view to the terminal's
// profile, so the styles here stay fully saturated and render deterministically.
var (
	// Spinner tint for the liveness indicator.
	styleSpinner = lipgloss.NewStyle().Foreground(lipgloss.Color("#7D56F4"))
	// Phase name, emphasized in bold.
	stylePhase = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7AA2F7"))
	// Marker introducing the target, muted so the name carries the color.
	styleTargetMark = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A8F98"))
	// Current org, project, or workspace target.
	styleTarget = lipgloss.NewStyle().Foreground(lipgloss.Color("#9ECE6A"))
	// Bytes, rate, and elapsed, in a muted tone.
	styleMeta = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A8F98"))
	// Unit-progress percent and eta beside the bar.
	styleCount = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A8F98"))

	// Done count, in green.
	styleDone = lipgloss.NewStyle().Foreground(lipgloss.Color("#9ECE6A"))
	// Errored count, in red.
	styleErrored = lipgloss.NewStyle().Foreground(lipgloss.Color("#F7768E"))
	// Forbidden count, in amber.
	styleForbidden = lipgloss.NewStyle().Foreground(lipgloss.Color("#E0AF68"))
	// Retried count, in cyan: neither success nor failure, but re-work the run
	// absorbed.
	styleRetried = lipgloss.NewStyle().Foreground(lipgloss.Color("#7DCFFF"))

	// Summary block heading.
	styleSummaryHead = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7AA2F7"))
	// Summary block's left rule, framing the block as one unit.
	styleSummaryRule = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder(), false, false, false, true).
				BorderForeground(lipgloss.Color("#7AA2F7")).
				PaddingLeft(1)

	// Resumed tag, dimmed so it reads as an aside.
	styleResumed = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#8A8F98"))

	// Rate-limited (429) readout, in the forbidden amber: not an error, but a
	// warning that explains a shrunken worker pool.
	styleRateLimited = lipgloss.NewStyle().Foreground(lipgloss.Color("#E0AF68"))

	// Ends of the blend the filled bar sweeps, spinner purple into phase blue,
	// shared with the indeterminate marquee so the two read as one component.
	barBlendStart = lipgloss.Color("#7D56F4")
	barBlendEnd   = lipgloss.Color("#7AA2F7")
	barEmptyColor = lipgloss.Color("#3B4261")

	// Track of the indeterminate marquee.
	styleBarTrack = lipgloss.NewStyle().Foreground(barEmptyColor)
)
