package progress

import (
	"charm.land/lipgloss/v2"

	"go.jacobcolvin.com/hcp_archiver/pkg/theme"
)

// Metadata glyphs leading the cells of the panel's footer grid, mirroring the
// status glyphs on the counts row so the footer's rows read as one grid. They
// are panel-specific (no other surface prints these readouts), so they live
// here rather than in the shared [theme] vocabulary. The arrows draw from two
// families the terminal renders as one set. The dashed arrows (⇠⇡⇢⇣) carry
// byte movement; the paired arrows with their bar (⇉⇇⇥) carry request flow:
// requests streaming out, the server pushing back with 429s, and a cooldown
// stopping the flow at a bar.
const (
	glyphBytes    = "⇣"
	glyphRate     = "⇢"
	glyphElapsed  = "◷"
	glyphRPS      = "⇉"
	glyph429      = "⇇"
	glyphPaused   = "⇥"
	glyphUploaded = "⇡"
	glyphEvicted  = "⇠"
)

// glyphCloud leads the remote-transfer row. U+2601 renders one or two cells
// depending on the terminal; a double-cell render shifts the row's remaining
// columns one cell right of the grid above, a drift tolerated as the row
// keeps its own cell rhythm and stays the frame's last line.
const glyphCloud = "☁"

// Surface styles mapping the panel's roles onto the shared [theme] tokens, so
// every color the panel renders traces to the one palette all surfaces share.
// Bubble Tea downsamples the composed view to the terminal's profile, so the
// truecolor tokens render deterministically.
var (
	// Spinner tint for the liveness indicator.
	styleSpinner = theme.Accent
	// Phase name, emphasized.
	stylePhase = theme.Heading
	// Marker introducing the target, muted so the name carries the color.
	styleTargetMark = theme.Muted
	// Current org, project, or workspace target.
	styleTarget = theme.Success
	// Bytes, rate, and elapsed.
	styleMeta = theme.Muted
	// Unit-progress percent and eta beside the bar.
	styleCount = theme.Muted

	// Per-status counts, one tone per outcome.
	styleDone      = theme.Success
	styleErrored   = theme.Error
	styleForbidden = theme.Warning
	// Retried is neither success nor failure, but re-work the run absorbed.
	styleRetried = theme.Info

	// Summary block heading.
	styleSummaryHead = theme.Heading
	// Summary block's left rule, framing the block as one unit.
	styleSummaryRule = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder(), false, false, false, true).
				BorderForeground(theme.ColorHeading).
				PaddingLeft(1)

	// Resumed tag, dimmed so it reads as an aside.
	styleResumed = theme.Muted.Faint(true)

	// Rate-limited (429) and cooldown-pause readouts: not an error, but a
	// warning that explains a slowed request rate.
	styleRateLimited = theme.Warning

	// Ends of the blend the filled bar sweeps, accent into heading, shared
	// with the indeterminate marquee so the two read as one component.
	barBlendStart = theme.ColorAccent
	barBlendEnd   = theme.ColorHeading
	barEmptyColor = theme.ColorTrack

	// Track of the indeterminate marquee.
	styleBarTrack = lipgloss.NewStyle().Foreground(theme.ColorTrack)
)
