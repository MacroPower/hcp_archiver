package view

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"go.jacobcolvin.com/hcp_archiver/theme"
)

// ErrBrowser wraps a browser runtime problem so callers can classify it with
// [errors.Is].
var ErrBrowser = errors.New("run archive browser")

// chromeLines is the vertical space the root model reserves above each screen:
// the breadcrumb line and the status line under it. The status line doubles as
// the loading indicator's row, so the chrome height is constant either way.
const chromeLines = 2

// loadingGrace is how long a screen build may run before the status line shows
// the loading spinner. Local builds settle well within it, so ordinary
// navigation never flickers; a build that outlives it is fetching from the
// remote store (or reading something comparably slow), which is exactly when
// feedback is due.
const loadingGrace = 150 * time.Millisecond

// Back keys: esc and backspace both pop a screen. While a screen build is in
// flight, the root model consumes esc to abandon the load instead of popping.
const (
	keyEsc       = "esc"
	keyBackspace = "backspace"
)

// Browse opens the archive at dir and drives the interactive browser on the
// given terminal streams until the user quits or ctx is canceled.
//
// A ctx cancellation (an external SIGINT) is returned as [context.Canceled]
// wrapped so the command can map it to a graceful exit. The same ctx bounds
// any remote bundle reads of an archive whose bundles were offloaded.
func Browse(ctx context.Context, dir string, in io.Reader, out io.Writer) error {
	orgs, err := OpenArchive(dir, WithContext(ctx))
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

// initializer is the optional screen hook for startup work: a screen that
// implements it has its init command run by the root model as it is pushed.
// Screens have no Bubble Tea Init of their own, so this is how an async
// screen ([*unsealProgressScreen]) starts its work and its first listen.
type initializer interface {
	init() tea.Cmd
}

// popMsg returns to the screen above; on the root screen it quits.
type popMsg struct{}

// statusMsg surfaces a non-fatal problem (an unreadable file, a malformed
// document) on the status line without leaving the current screen.
type statusMsg struct {
	err error
}

// loadStartMsg announces a screen build entering flight: the root model counts
// it, schedules the loading indicator's grace timer, and runs build, whose
// result comes back as a [loadDoneMsg].
type loadStartMsg struct {
	build tea.Cmd
}

// loadGraceMsg fires when [loadingGrace] elapses; a build still in flight then
// turns the status line into a spinner.
type loadGraceMsg struct{}

// loadDoneMsg settles one screen build, carrying its outcome (a [pushMsg] or a
// [statusMsg]), which the root model applies after clearing the indicator. The
// epoch is stamped by the root model as the build launches; an outcome whose
// epoch has gone stale belongs to an abandoned load and is discarded.
type loadDoneMsg struct {
	msg   tea.Msg
	epoch int
}

// model is the root Bubble Tea model: a stack of screens under a breadcrumb.
//
// Create instances with [newModel].
type model struct {
	status string
	stack  []screen
	// The loading indicator: spin renders on the status line while a screen
	// build outlives loadingGrace, loading counts the in-flight builds,
	// spinning marks the indicator visible, and epoch numbers the current
	// generation of builds so an abandoned one settles into the void.
	spin     spinner.Model
	width    int
	height   int
	loading  int
	spinning bool
	epoch    int
}

// newModel creates a new [model] opening at the organization list, or directly
// at the organization's sections when the archive holds only one.
func newModel(orgs []*Org) *model {
	root := newOrgsScreen(orgs)
	if len(orgs) == 1 {
		root = newOrgScreen(orgs[0])
	}

	spin := spinner.New(spinner.WithSpinner(spinner.Dot))
	spin.Style = theme.Accent

	return &model{stack: []screen{root}, spin: spin}
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

		if i, ok := msg.s.(initializer); ok {
			return m, i.init()
		}

		return m, nil

	case popMsg:
		// Leaving a screen abandons any load launched from it: the built
		// screen would otherwise push into a spot on the stack that no longer
		// means what it did when the load started.
		m.abandonLoads()

		if len(m.stack) == 1 {
			return m, tea.Quit
		}

		m.stack = m.stack[:len(m.stack)-1]
		m.status = ""

		return m, nil

	case statusMsg:
		m.status = msg.err.Error()

		return m, nil

	case loadStartMsg:
		m.loading++

		build := stampEpoch(msg.build, m.epoch)

		// A build joining one already in flight rides the earlier grace timer
		// (or the spinner it already showed); only the first arms a fresh one.
		if m.loading > 1 {
			return m, build
		}

		grace := tea.Tick(loadingGrace, func(time.Time) tea.Msg { return loadGraceMsg{} })

		return m, tea.Batch(build, grace)

	case loadGraceMsg:
		// A grace timer is never canceled, so one armed by a settled load can
		// still fire while a later load is in flight. Starting a chain from it
		// while the spinner already ticks would leave two chains running, each
		// re-arming the other's frame: the indicator would spin at twice the
		// rate and the pair would never rejoin. Only a grace that finds the
		// indicator dark starts one.
		if m.loading == 0 || m.spinning {
			return m, nil
		}

		m.spinning = true

		return m, m.spin.Tick

	case loadDoneMsg:
		// A stale epoch is a load abandoned while it was in flight: the user
		// has walked away, so the outcome is dropped rather than applied.
		if msg.epoch != m.epoch {
			return m, nil
		}

		m.loading = max(m.loading-1, 0)
		if m.loading == 0 {
			m.spinning = false
		}

		return m.Update(msg.msg)

	case spinner.TickMsg:
		if msg.ID == m.spin.ID() {
			// A tick landing after its load settled ends the chain; the next
			// grace starts a fresh one.
			if !m.spinning {
				return m, nil
			}

			var cmd tea.Cmd

			m.spin, cmd = m.spin.Update(msg)

			return m, cmd
		}

		// Another spinner's tick (an unseal run's) belongs to the screen below.

	case tea.KeyPressMsg:
		m.status = ""

		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}

		// Esc during a screen build abandons the load and stays put; the key
		// is consumed so the screen below does not also pop. The build itself
		// (a remote fetch, perhaps) runs on to completion or timeout in the
		// background; only its outcome is discarded.
		if msg.String() == keyEsc && m.loading > 0 {
			m.abandonLoads()

			return m, nil
		}
	}

	return m, m.top().update(msg)
}

// View renders the breadcrumb, the status line, and the active screen in the
// alternate screen buffer.
//
// The breadcrumb and status are each clamped to one physical row -- the
// breadcrumb to m.width, the status to m.width and one line -- so the chrome
// occupies exactly [chromeLines] rows: a deep trail or a multi-line error
// string would otherwise wrap past its row and push the content's bottom off
// the terminal, whose height is sized as height minus [chromeLines]. MaxWidth
// truncates without padding and is a no-op at width zero (the first frame,
// before the terminal size is known), so an unsized breadcrumb still renders.
//
// A screen build that outlives [loadingGrace] shows the loading spinner on the
// status row; an error takes precedence, so the two never stack.
func (m *model) View() tea.View {
	var b strings.Builder

	b.WriteString(lipgloss.NewStyle().MaxWidth(m.width).Render(m.breadcrumb()))
	b.WriteByte('\n')

	switch {
	case m.status != "":
		b.WriteString(styleStatusErr.MaxWidth(m.width).MaxHeight(1).Render(m.status))
	case m.spinning:
		b.WriteString(lipgloss.NewStyle().MaxWidth(m.width).Render(
			m.spin.View() + styleFooter.Render("loading...")))
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

	return strings.Join(crumbs, styleCrumbSep.Render(" "+theme.GlyphCrumb+" "))
}

// abandonLoads walks away from every in-flight screen build: the epoch
// advances past theirs, so each settles into a discarded [loadDoneMsg], and
// the loading indicator clears immediately.
func (m *model) abandonLoads() {
	m.epoch++
	m.loading = 0
	m.spinning = false
}

// push wraps a screen constructor into a command, surfacing a construction
// problem on the status line instead of descending. A constructor may fetch
// from the remote store, so the command announces the load first; the root
// model counts it, shows a spinner if the build outlives [loadingGrace], and
// runs the build itself.
func push(open func() (screen, error)) tea.Cmd {
	return func() tea.Msg {
		return loadStartMsg{build: buildScreen(open)}
	}
}

// buildScreen runs a pushed screen's constructor, settling its load with the
// new screen or with the construction error bound for the status line.
func buildScreen(open func() (screen, error)) tea.Cmd {
	return func() tea.Msg {
		s, err := open()
		if err != nil {
			return loadDoneMsg{msg: statusMsg{err: err}}
		}

		return loadDoneMsg{msg: pushMsg{s: s}}
	}
}

// stampEpoch marks a build's settling message with the epoch it launched
// under, so the root model can tell a live outcome from one whose load was
// abandoned mid-flight.
func stampEpoch(build tea.Cmd, epoch int) tea.Cmd {
	return func() tea.Msg {
		msg := build()
		if done, ok := msg.(loadDoneMsg); ok {
			done.epoch = epoch

			return done
		}

		return msg
	}
}

// pop returns the command that leaves the current screen.
func pop() tea.Cmd {
	return func() tea.Msg {
		return popMsg{}
	}
}
