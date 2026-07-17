package view

import "go.jacobcolvin.com/hcp_archiver/theme"

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
