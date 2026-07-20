package view

import (
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"go.jacobcolvin.com/niceyaml"
	"go.jacobcolvin.com/niceyaml/style"

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

// newThemedPrinter creates a niceyaml printer whose syntax palette resolves to
// the shared [theme] tokens. The yamlviewport's default printer carries
// niceyaml's charm theme, whose colors (and solid background) belong to a
// palette unrelated to the tool's; every viewer viewport is built over this
// printer instead, so the document body renders in the same palette as the
// chrome around it.
func newThemedPrinter() *niceyaml.Printer {
	// The base style inherits the terminal's own foreground and background,
	// like every other surface in the tool; the charm theme's painted
	// background is deliberately dropped.
	base := lipgloss.NewStyle()

	return niceyaml.NewPrinter(niceyaml.WithStyles(style.NewStyles(base,
		// Comment also tints the gutter: the printer derives its line numbers
		// from the comment foreground, landing them in the same muted tone as
		// the footer and key hints.
		style.Set(style.Comment, theme.Muted),
		// Mapping keys are the document's structure, so they take the heading
		// tone; unbolded, because a whole document of bold keys would shout.
		style.Set(style.NameTag, base.Foreground(theme.ColorHeading)),
		// Anchors, aliases, and tags are cross-references, so they take the
		// liveness accent.
		style.Set(style.Name, base.Foreground(theme.ColorAccent)),
		style.Set(style.LiteralString, base.Foreground(theme.ColorSuccess)),
		style.Set(style.LiteralNumber, base.Foreground(theme.ColorWarning)),
		style.Set(style.LiteralBoolean, base.Foreground(theme.ColorInfo)),
		style.Set(style.LiteralNull, base.Foreground(theme.ColorInfo)),
		style.Set(style.Punctuation, theme.Muted),
		style.Set(style.TextAccent, theme.Accent),
		style.Set(style.TextSubtle, theme.Muted),
		style.Set(style.TextSubtleDim, base.Foreground(theme.ColorTrack)),
		style.Set(style.TextOK, theme.Success),
		style.Set(style.TextWarn, theme.Warning),
		style.Set(style.TextError, theme.Error),
		style.Set(style.GenericError, theme.Error),
		style.Set(style.GenericInserted, theme.Success),
		style.Set(style.GenericDeleted, theme.Error),
		style.Set(style.GenericHeading, theme.Heading),
		// Search matches overlay the tokens: the selected match inverts the
		// terminal's own colors (no off-palette hue), the rest sit on the
		// inactive-structure track tone.
		style.Set(style.GenericHighlight, base.Reverse(true)),
		style.Set(style.GenericHighlightDim, base.Background(theme.ColorTrack)),
	)))
}

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

	// The filter input carries its own style set, themed like every other text
	// input in the browser.
	fs := l.FilterInput.Styles()
	themeInputStyles(&fs)
	l.FilterInput.SetStyles(fs)

	return l
}

// newThemedTable creates a static display table whose chrome draws from the
// shared [theme]. Every browser table is built here, so no screen can fall
// back to the bubbles defaults, whose pink selected row and padded cells
// belong to a palette and layout unrelated to the tool's. The table is
// unfocused and its Selected style is neutralized: a blurred table still
// paints its cursor row, and no row of a display table may look selected. The
// cells carry no padding, so column widths land exactly where the caller's
// width math puts them; any inter-column gap belongs in the column widths.
func newThemedTable(cols []table.Column, rows []table.Row) table.Model {
	return table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithFocused(false),
		table.WithStyles(table.Styles{
			Header:   theme.Heading,
			Cell:     lipgloss.NewStyle(),
			Selected: lipgloss.NewStyle(),
		}),
	)
}

// themeInputStyles maps a text input's style set onto the shared [theme]: the
// prompt takes the heading tone the rest of the chrome uses, and the cursor
// the liveness accent. Every browser text input is styled here, so the inputs
// cannot drift apart.
func themeInputStyles(s *textinput.Styles) {
	s.Focused.Prompt = theme.Heading
	s.Blurred.Prompt = theme.Heading
	s.Cursor.Color = theme.ColorAccent
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
