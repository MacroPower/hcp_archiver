package view

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// ErrBrowser wraps a browser runtime problem so callers can classify it with
// [errors.Is].
var ErrBrowser = errors.New("run archive browser")

// chromeLines is the vertical space the root model reserves above each screen:
// the breadcrumb line and the status line under it.
const chromeLines = 2

// Browse opens the archive at dir and drives the interactive browser on the
// given terminal streams until the user quits or ctx is canceled.
//
// A ctx cancellation (an external SIGINT) is returned as [context.Canceled]
// wrapped so the command can map it to a graceful exit.
func Browse(ctx context.Context, dir string, in io.Reader, out io.Writer) error {
	orgs, err := OpenArchive(dir)
	if err != nil {
		return err
	}

	program := tea.NewProgram(
		newModel(orgs),
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(out),
	)

	_, err = program.Run()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %w", ErrBrowser, ctx.Err())
		}

		return fmt.Errorf("%w: %w", ErrBrowser, err)
	}

	return nil
}

// screen is one level of the browser: a list, a detail menu, or a content
// viewer. The root model keeps a stack of these and renders the top one under
// the breadcrumb built from every crumb on the stack.
type screen interface {
	// Update handles one message and returns a command; navigation is signaled
	// by commands producing [pushMsg] and [popMsg].
	update(msg tea.Msg) tea.Cmd
	// View renders the screen's body.
	view() string
	// Crumb names the screen's breadcrumb segment.
	crumb() string
	// SetSize gives the screen its body dimensions.
	setSize(width, height int)
}

// pushMsg descends into a deeper screen.
type pushMsg struct {
	s screen
}

// popMsg returns to the screen above; on the root screen it quits.
type popMsg struct{}

// statusMsg surfaces a non-fatal problem (an unreadable file, a malformed
// document) on the status line without leaving the current screen.
type statusMsg struct {
	err error
}

// model is the root Bubble Tea model: a stack of screens under a breadcrumb.
//
// Create instances with [newModel].
type model struct {
	status string
	stack  []screen
	width  int
	height int
}

// newModel creates a new [model] opening at the organization list, or directly
// at the organization's sections when the archive holds only one.
func newModel(orgs []*Org) *model {
	root := newOrgsScreen(orgs)
	if len(orgs) == 1 {
		root = newOrgScreen(orgs[0])
	}

	return &model{stack: []screen{root}}
}

// Init performs no startup work; screens load their content when pushed.
func (m *model) Init() tea.Cmd {
	return nil
}

// top returns the active screen.
func (m *model) top() screen {
	return m.stack[len(m.stack)-1]
}

// Update routes messages: navigation and sizing are handled here, everything
// else goes to the active screen. Any key press clears the status line, so a
// stale error never outlives the interaction that caused it.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		for _, s := range m.stack {
			s.setSize(msg.Width, max(msg.Height-chromeLines, 0))
		}

		return m, nil

	case pushMsg:
		msg.s.setSize(m.width, max(m.height-chromeLines, 0))

		m.stack = append(m.stack, msg.s)
		m.status = ""

		return m, nil

	case popMsg:
		if len(m.stack) == 1 {
			return m, tea.Quit
		}

		m.stack = m.stack[:len(m.stack)-1]
		m.status = ""

		return m, nil

	case statusMsg:
		m.status = msg.err.Error()

		return m, nil

	case tea.KeyPressMsg:
		m.status = ""

		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}

	return m, m.top().update(msg)
}

// View renders the breadcrumb, the status line, and the active screen in the
// alternate screen buffer.
func (m *model) View() tea.View {
	var b strings.Builder

	b.WriteString(m.breadcrumb())
	b.WriteByte('\n')

	if m.status != "" {
		b.WriteString(styleStatusErr.Render(m.status))
	}

	b.WriteByte('\n')
	b.WriteString(m.top().view())

	v := tea.NewView(b.String())
	v.AltScreen = true

	return v
}

// breadcrumb joins every stacked screen's crumb into the header trail.
func (m *model) breadcrumb() string {
	crumbs := make([]string, 0, len(m.stack))

	for _, s := range m.stack {
		if c := s.crumb(); c != "" {
			crumbs = append(crumbs, styleCrumb.Render(c))
		}
	}

	return strings.Join(crumbs, styleCrumbSep.Render(" › "))
}

// push wraps a screen constructor into a command, surfacing a construction
// problem on the status line instead of descending.
func push(open func() (screen, error)) tea.Cmd {
	return func() tea.Msg {
		s, err := open()
		if err != nil {
			return statusMsg{err: err}
		}

		return pushMsg{s: s}
	}
}

// pop returns the command that leaves the current screen.
func pop() tea.Cmd {
	return func() tea.Msg {
		return popMsg{}
	}
}
