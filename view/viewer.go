package view

import (
	"fmt"

	"charm.land/bubbles/v2/viewport"

	tea "charm.land/bubbletea/v2"
)

// scrollKey handles the navigation keys the viewer screens share -- back, quit,
// and jump to top or bottom -- returning the command and whether it consumed the
// key. The top and bottom callbacks scroll the caller's own viewport, whose
// GotoTop/GotoBottom signatures differ between the plain and highlighting
// viewports and whose Update returns its concrete type, so only the key handling
// is shared here. A key it does not recognize is left for the caller to forward
// to its viewport.
func scrollKey(msg tea.Msg, top, bottom func()) (tea.Cmd, bool) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil, false
	}

	switch key.String() {
	case keyEsc, keyBackspace:
		return pop(), true
	case "q":
		return tea.Quit, true
	case "g", "home":
		top()

		return nil, true

	case "G", "end":
		bottom()

		return nil, true
	}

	return nil, false
}

// viewerScreen scrolls one archived plain-text document: a log or the mixed
// text-and-JSON overview. JSON documents go through [yamlViewerScreen]
// instead, which adds syntax highlighting.
//
// Create instances with [newViewerScreen].
type viewerScreen struct {
	name string
	vp   viewport.Model
}

// newViewerScreen creates a new [viewerScreen] named name (its breadcrumb
// segment) over content. Long lines soft-wrap, so a log line or a minified
// document stays readable without horizontal scrolling.
func newViewerScreen(name, content string) *viewerScreen {
	vp := viewport.New()
	vp.SoftWrap = true
	vp.SetContent(content)

	return &viewerScreen{name: name, vp: vp}
}

// update handles navigation keys itself and forwards scrolling to the
// viewport.
func (s *viewerScreen) update(msg tea.Msg) tea.Cmd {
	if cmd, handled := scrollKey(msg, func() { s.vp.GotoTop() }, func() { s.vp.GotoBottom() }); handled {
		return cmd
	}

	var cmd tea.Cmd

	s.vp, cmd = s.vp.Update(msg)

	return cmd
}

// view renders the viewport over a footer carrying the scroll position and the
// key hints.
func (s *viewerScreen) view() string {
	footer := fmt.Sprintf("%3.f%% · ↑/↓ scroll · g/G top/bottom · esc back · q quit",
		s.vp.ScrollPercent()*100)

	return s.vp.View() + "\n" + styleFooter.Render(footer)
}

// crumb names the screen's breadcrumb segment.
func (s *viewerScreen) crumb() string { return s.name }

// setSize sizes the viewport to the screen body, minus its footer line.
func (s *viewerScreen) setSize(width, height int) {
	s.vp.SetWidth(width)
	s.vp.SetHeight(max(height-1, 0))
}
