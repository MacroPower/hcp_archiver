package view

import (
	"fmt"
	"strings"

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

// viewerFooter renders the footer line the viewer screens share: the scroll
// position, then the key hints — the keys scrollKey handles, with any
// viewer-specific hints spliced in before the back and quit keys — so the
// viewers cannot drift in hint wording or order. It lives beside scrollKey
// because the two describe the same keys.
func viewerFooter(scrollPercent float64, extra ...string) string {
	hints := append([]string{
		fmt.Sprintf("%3.f%%", scrollPercent*100),
		"↑/↓ scroll",
		"g/G top/bottom",
	}, extra...)
	hints = append(hints, "esc back", "q quit")

	return styleFooter.Render(strings.Join(hints, " · "))
}

// viewerScreen scrolls one archived plain-text document (a log). JSON
// documents go through [yamlViewerScreen] instead, which adds syntax
// highlighting.
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
	return s.vp.View() + "\n" + viewerFooter(s.vp.ScrollPercent())
}

// crumb names the screen's breadcrumb segment.
func (s *viewerScreen) crumb() string { return s.name }

// setSize sizes the viewport to the screen body, minus its footer line.
func (s *viewerScreen) setSize(width, height int) {
	s.vp.SetWidth(width)
	s.vp.SetHeight(max(height-1, 0))
}
