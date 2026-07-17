package theme

import "charm.land/lipgloss/v2"

// Palette tokens, named for the role a color plays rather than its hue. Every
// color any surface renders resolves to one of these, so retuning a token
// restyles the whole tool at once and two surfaces can never disagree about
// what, say, a warning looks like.
var (
	// ColorAccent tints the liveness accents: the spinner and the leading end
	// of the progress-bar blend.
	ColorAccent = lipgloss.Color("#7D56F4")
	// ColorHeading tints emphasized structure: headings, breadcrumbs, the
	// phase name, and the trailing end of the progress-bar blend.
	ColorHeading = lipgloss.Color("#7AA2F7")
	// ColorMuted tints secondary text: metadata, separators, footers, and key
	// hints.
	ColorMuted = lipgloss.Color("#8A8F98")
	// ColorSuccess tints completed work and the names it landed on.
	ColorSuccess = lipgloss.Color("#9ECE6A")
	// ColorError tints failures.
	ColorError = lipgloss.Color("#F7768E")
	// ColorWarning tints degraded-but-not-failed states: forbidden work,
	// rate limiting, cooldown pauses.
	ColorWarning = lipgloss.Color("#E0AF68")
	// ColorInfo tints neutral asides that are neither success nor failure,
	// such as retried work.
	ColorInfo = lipgloss.Color("#7DCFFF")
	// ColorTrack tints inactive structure, such as the empty run of a
	// progress bar.
	ColorTrack = lipgloss.Color("#3B4261")

	// Shared text styles, one per palette role every surface uses. Packages
	// derive surface-specific variants (padding, borders, faintness) from
	// these; a lipgloss style is a value, so deriving never mutates the
	// shared definition.

	// Heading emphasizes structural labels: bold in the heading tone.
	Heading = lipgloss.NewStyle().Bold(true).Foreground(ColorHeading)
	// Accent renders the liveness accents.
	Accent = lipgloss.NewStyle().Foreground(ColorAccent)
	// Muted renders secondary text.
	Muted = lipgloss.NewStyle().Foreground(ColorMuted)
	// Success renders completed work.
	Success = lipgloss.NewStyle().Foreground(ColorSuccess)
	// Error renders failures.
	Error = lipgloss.NewStyle().Foreground(ColorError)
	// Warning renders degraded-but-not-failed states.
	Warning = lipgloss.NewStyle().Foreground(ColorWarning)
	// Info renders neutral asides.
	Info = lipgloss.NewStyle().Foreground(ColorInfo)
)

// Status glyphs, one per outcome, so the same outcome wears the same mark on
// every surface: the panel's per-status counts, the summary heading, and the
// browser's run badges all draw from this vocabulary.
const (
	// GlyphOK marks completed, successful work.
	GlyphOK = "✓"
	// GlyphError marks failed work.
	GlyphError = "✗"
	// GlyphBlocked marks work refused or discarded before it could finish.
	GlyphBlocked = "⊘"
	// GlyphRetry marks re-work absorbed along the way.
	GlyphRetry = "↻"
	// GlyphActive marks work still in motion.
	GlyphActive = "…"
	// GlyphNeutral marks a state carrying no outcome signal.
	GlyphNeutral = "•"
	// GlyphPartial marks a partial outcome, such as a plan that never applied.
	GlyphPartial = "◇"
)

// Chrome glyphs shared by the surfaces' framing.
const (
	// GlyphPointer introduces a target or item name.
	GlyphPointer = "▸"
	// GlyphCrumb separates breadcrumb segments.
	GlyphCrumb = "›"
)
