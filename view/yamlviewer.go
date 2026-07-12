package view

import (
	"fmt"

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
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case keyEsc, keyBackspace:
			return pop()
		case "q":
			return tea.Quit
		case "g", "home":
			s.vp.GotoTop()

			return nil

		case "G", "end":
			s.vp.GotoBottom()

			return nil
		}
	}

	var cmd tea.Cmd

	s.vp, cmd = s.vp.Update(msg)

	return cmd
}

// view renders the viewport over a footer carrying the scroll position and the
// key hints.
func (s *yamlViewerScreen) view() string {
	footer := fmt.Sprintf("%3.f%% · ↑/↓ scroll · g/G top/bottom · w wrap · esc back · q quit",
		s.vp.ScrollPercent()*100)

	return s.vp.View() + "\n" + styleFooter.Render(footer)
}

// crumb names the screen's breadcrumb segment.
func (s *yamlViewerScreen) crumb() string { return s.name }

// setSize sizes the viewport to the screen body, minus its footer line.
func (s *yamlViewerScreen) setSize(width, height int) {
	s.vp.SetWidth(width)
	s.vp.SetHeight(max(height-1, 0))
}
