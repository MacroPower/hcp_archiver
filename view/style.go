package view

import (
	"fmt"

	"charm.land/lipgloss/v2"
)

// Palette for the browser chrome, matching the progress panel's colors so the
// two surfaces read as one tool.
var (
	// Breadcrumb trail across the top of the screen.
	styleCrumb = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7AA2F7"))
	// Separator between breadcrumb segments.
	styleCrumbSep = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A8F98"))
	// Transient error line under the breadcrumb.
	styleStatusErr = lipgloss.NewStyle().Foreground(lipgloss.Color("#F7768E"))
	// Key hints and scroll position in the viewer footer.
	styleFooter = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A8F98"))
)

// Run-status glyphs, mirroring how the HCP interface badges a run. They ride
// inside plain list-item text (the list delegate owns the item's styling), so
// the glyph shape, not color, carries the status.
const (
	glyphRunOK       = "✓"
	glyphRunErr      = "✗"
	glyphRunDiscard  = "⊘"
	glyphRunActive   = "…"
	glyphRunNeutral  = "•"
	glyphRunPlanOnly = "◇"
)

// runGlyph maps an archived run status to its badge glyph.
func runGlyph(status string) string {
	switch status {
	case "applied", "planned_and_finished":
		return glyphRunOK
	case "errored":
		return glyphRunErr
	case "discarded", "canceled", "force_canceled":
		return glyphRunDiscard
	case "planned", "planning", "applying", "pending", "plan_queued",
		"apply_queued", "queuing", "fetching", "confirmed", "post_plan_running",
		"pre_plan_running", "policy_checking", "cost_estimating":
		return glyphRunActive
	case "policy_override", "policy_soft_failed":
		return glyphRunPlanOnly
	default:
		return glyphRunNeutral
	}
}

// humanBytes renders a byte count in a compact unit for list descriptions.
func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	const prefixes = "KMGTPE"

	value, exp := float64(n)/unit, 0 // exp indexes prefixes; 0 is KB (one divide).
	for value >= unit && exp < len(prefixes)-1 {
		value /= unit
		exp++
	}

	// Rounding to one decimal can lift a value just under the next unit up to a
	// bare "1024.0"; promote it so the label stays compact (1.0 MB, not
	// 1024.0 KB). The 0.05 margin is half the printed precision.
	if value >= unit-0.05 && exp < len(prefixes)-1 {
		value /= unit
		exp++
	}

	return fmt.Sprintf("%.1f %cB", value, prefixes[exp])
}
