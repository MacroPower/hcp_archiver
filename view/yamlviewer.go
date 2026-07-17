package view

import (
	"go.jacobcolvin.com/niceyaml"
	"go.jacobcolvin.com/niceyaml/bubbles/yamlviewport"

	tea "charm.land/bubbletea/v2"
)

// yamlViewerScreen scrolls one archived JSON document through the niceyaml
// viewport, which lexes it as YAML (JSON is a YAML subset) and renders it with
// syntax highlighting and line numbers.
//
// Create instances with [newYAMLViewerScreen].
type yamlViewerScreen struct {
	name string
	vp   yamlviewport.Model
}

// newYAMLViewerScreen creates a new [yamlViewerScreen] named name (its
// breadcrumb segment) over content. Long lines wrap by default (w toggles), so
// a minified document stays readable without horizontal scrolling.
func newYAMLViewerScreen(name, content string) *yamlViewerScreen {
	vp := yamlviewport.New()
	vp.FillHeight = true
	vp.SetTokens(niceyaml.NewSourceFromString(content, niceyaml.WithName(name)))

	// A single document has no revision history, so the revision and diff keys
	// (tab, m, v) would only toggle the viewport into confusing empty states.
	vp.KeyMap.NextRevision.SetEnabled(false)
	vp.KeyMap.PrevRevision.SetEnabled(false)
	vp.KeyMap.ToggleDiffMode.SetEnabled(false)
	vp.KeyMap.ToggleViewMode.SetEnabled(false)

	return &yamlViewerScreen{name: name, vp: vp}
}

// update handles navigation keys itself and forwards scrolling to the
// viewport.
func (s *yamlViewerScreen) update(msg tea.Msg) tea.Cmd {
	if cmd, handled := scrollKey(msg, func() { s.vp.GotoTop() }, func() { s.vp.GotoBottom() }); handled {
		return cmd
	}

	var cmd tea.Cmd

	s.vp, cmd = s.vp.Update(msg)

	return cmd
}

// view renders the viewport over a footer carrying the scroll position and the
// key hints.
func (s *yamlViewerScreen) view() string {
	return s.vp.View() + "\n" + viewerFooter(s.vp.ScrollPercent(), "w wrap")
}

// crumb names the screen's breadcrumb segment.
func (s *yamlViewerScreen) crumb() string { return s.name }

// setSize sizes the viewport to the screen body, minus its footer line.
func (s *yamlViewerScreen) setSize(width, height int) {
	s.vp.SetWidth(width)
	s.vp.SetHeight(max(height-1, 0))
}
