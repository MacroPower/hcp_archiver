package view

import (
	"charm.land/bubbles/v2/list"
	"charm.land/lipgloss/v2"

	"go.jacobcolvin.com/hcp_archiver/theme"
)

// Surface styles mapping the browser chrome's roles onto the shared [theme]
// tokens, so the browser and the progress panel read as one tool by
// construction rather than by keeping two palettes in step by hand.
var (
	// Breadcrumb trail across the top of the screen.
	styleCrumb = theme.Heading
	// Separator between breadcrumb segments.
	styleCrumbSep = theme.Muted
	// Transient error line under the breadcrumb.
	styleStatusErr = theme.Error
	// Key hints and scroll position in the viewer footer.
	styleFooter = theme.Muted
)

// newThemedList creates a list whose delegate and chrome draw from the shared
// [theme]. Every browser list is built here, so no screen can fall back to the
// bubbles defaults, whose pink-and-purple selection and assorted grays are a
// palette unrelated to the tool's. Only the colors change; the delegate's
// spacing, borders, and dimming behavior stay stock.
func newThemedList(entries []list.Item) list.Model {
	track := lipgloss.NewStyle().Foreground(theme.ColorTrack)

	d := list.NewDefaultDelegate()
	d.Styles.SelectedTitle = d.Styles.SelectedTitle.
		Foreground(theme.ColorHeading).
		BorderForeground(theme.ColorHeading)
	d.Styles.SelectedDesc = d.Styles.SelectedDesc.
		Foreground(theme.ColorMuted).
		BorderForeground(theme.ColorHeading)
	d.Styles.NormalDesc = d.Styles.NormalDesc.Foreground(theme.ColorMuted)

	l := list.New(entries, d, 0, 0)
	l.Styles.Spinner = theme.Accent
	l.Styles.StatusBar = l.Styles.StatusBar.Foreground(theme.ColorMuted)
	l.Styles.StatusEmpty = l.Styles.StatusEmpty.Foreground(theme.ColorMuted)
	l.Styles.StatusBarFilterCount = l.Styles.StatusBarFilterCount.Foreground(theme.ColorTrack)
	l.Styles.NoItems = l.Styles.NoItems.Foreground(theme.ColorMuted)
	l.Styles.DividerDot = l.Styles.DividerDot.Foreground(theme.ColorTrack)

	// The paginator copies its dot strings out of the styles at construction,
	// so restyling it means setting the rendered dots directly.
	l.Paginator.ActiveDot = theme.Muted.Render(theme.GlyphNeutral)
	l.Paginator.InactiveDot = track.Render(theme.GlyphNeutral)

	// The filter input carries its own style set; the prompt takes the heading
	// tone the rest of the chrome uses, and the cursor the liveness accent.
	fs := l.FilterInput.Styles()
	fs.Focused.Prompt = theme.Heading
	fs.Blurred.Prompt = theme.Heading
	fs.Cursor.Color = theme.ColorAccent
	l.FilterInput.SetStyles(fs)

	return l
}

// runGlyph maps an archived run status to its badge glyph, mirroring how the
// HCP interface badges a run. The glyphs ride inside plain list-item text
// (the list delegate owns the item's styling), so the glyph shape, not color,
// carries the status.
func runGlyph(status string) string {
	switch status {
	case "applied", "planned_and_finished":
		return theme.GlyphOK
	case "errored":
		return theme.GlyphError
	case "discarded", "canceled", "force_canceled":
		return theme.GlyphBlocked
	case "planned", "planning", "applying", "pending", "plan_queued",
		"apply_queued", "queuing", "fetching", "confirmed", "post_plan_running",
		"pre_plan_running", "policy_checking", "cost_estimating":
		return theme.GlyphActive
	case "policy_override", "policy_soft_failed":
		return theme.GlyphPartial
	default:
		return theme.GlyphNeutral
	}
}
