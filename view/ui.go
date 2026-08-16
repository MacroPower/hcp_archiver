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
// given terminal streams until the user quits or ctx is canceled. Additional
// [ArchiveOption]s (a [WithRemote] mirror, say) pass through to [OpenArchive];
// the browse context always wins.
//
// A ctx cancellation (an external SIGINT) is returned as [context.Canceled]
// wrapped so the command can map it to a graceful exit. The same ctx bounds
// any remote reads of an archive whose objects live in its mirror.
//
//nolint:contextcheck // The browse context rides in through WithContext; remote reads derive from it.
func Browse(ctx context.Context, dir string, in io.Reader, out io.Writer, opts ...ArchiveOption) error {
	openOpts := make([]ArchiveOption, 0, len(opts)+1)
	openOpts = append(openOpts, opts...)
	openOpts = append(openOpts, WithContext(ctx))

	orgs, err := OpenArchive(dir, openOpts...)
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
// screen ([*extractProgressScreen]) starts its work and its first listen.
type initializer interface {
	init() tea.Cmd
}

// closer is the optional screen hook for teardown: a screen owning something
// that outlives its own update loop (an extract run's goroutine and the context
// bounding it) implements it, and the root model closes the screen as it
// leaves the stack. Screens never see the quit that ends the program, so this
// is the only place such a run can be stopped.
//
// See [*extractProgressScreen] for an implementation.
type closer interface {
	close()
}

// popMsg returns to the screen above; on the root screen it quits.
type popMsg struct{}

// quitMsg ends the browser. Screens signal it rather than returning tea.Quit
// themselves so every exit runs the root model's teardown first: the runtime
// swallows a quit command without ever routing it back through Update, and a
// screen torn down by the program's exit alone would strand whatever it owns.
type quitMsg struct{}

// statusMsg surfaces a non-fatal problem (an unreadable file, a malformed
// document) on the status line without leaving the current screen.
type statusMsg struct {
	err error
}

// loadStartMsg announces a screen build entering flight: the root model counts
// it, schedules the loading indicator's grace timer, and runs build, whose
// result comes back as a [loadDoneMsg]. The epoch is the load generation
// current where the announcing command was dispatched, not where it lands: a
// pop or an abandon can be processed in between, and the load belongs to the
// stack the user was looking at when they launched it.
type loadStartMsg struct {
	build tea.Cmd
	epoch int
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
	// The archive's organizations, held for the one-time degraded-mirror
	// warning: remoteWarned marks that the status line has already said the
	// mirror could not be listed, so an offline session hears it once rather
	// than on every screen.
	orgs []*Org
	// The loading indicator: spin renders on the status line while a screen
	// build outlives loadingGrace, loading counts the in-flight builds,
	// spinning marks the indicator visible, and epoch numbers the current
	// generation of builds so an abandoned one settles into the void.
	spin         spinner.Model
	width        int
	height       int
	loading      int
	spinning     bool
	remoteWarned bool
	epoch        int
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

	return &model{stack: []screen{root}, orgs: orgs, spin: spin}
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
		// Descending abandons every other in-flight build for the same reason
		// popping does: they were launched from the screen this one now
		// covers, so their result would land on a stack that no longer means
		// what it did. It is also what keeps an impatient second keypress
		// during a slow build from stacking the same screen twice.
		m.abandonLoads()

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
			cmd := m.teardown()

			return m, cmd
		}

		closeScreen(m.top())

		m.stack = m.stack[:len(m.stack)-1]
		m.status = ""

		return m, nil

	case quitMsg:
		cmd := m.teardown()

		return m, cmd

	case statusMsg:
		m.status = msg.err.Error()

		return m, nil

	case loadStartMsg:
		// A stale epoch is a load the user walked away from between the
		// keypress that launched it and this announcement: the screen it was
		// launched from is gone or covered, so the build never even starts.
		if msg.epoch != m.epoch {
			return m, nil
		}

		m.loading++

		build := stampDone(msg.build, m.epoch)

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
		// has walked away, so the outcome is dropped rather than applied. The
		// screen it built still exists and may already own a cancelable run's
		// context, so it is closed on its way to the void.
		if msg.epoch != m.epoch {
			if built, ok := msg.msg.(pushMsg); ok {
				closeScreen(built.s)
			}

			return m, nil
		}

		m.loading = max(m.loading-1, 0)
		if m.loading == 0 {
			m.spinning = false
		}

		mdl, cmd := m.Update(msg.msg)
		m.noteRemoteWarning()

		return mdl, cmd

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

		// Another spinner's tick (an extract run's) belongs to the screen below.

	case tea.KeyPressMsg:
		m.status = ""

		if msg.String() == "ctrl+c" {
			cmd := m.teardown()

			return m, cmd
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

	// Whatever the active screen asks for is dispatched under the epoch it
	// asked under, so a load it launches is judged against the stack as it
	// stood at the keypress rather than as it stands when the announcement
	// happens to arrive.
	return m, stampStart(m.top().update(msg), m.epoch)
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

// noteRemoteWarning surfaces a degraded mirror listing on the status line,
// once per session: the screen the user just loaded may be showing local
// content only, and saying so beats letting a partial tree read as the whole
// archive. It runs after a settled load so the first screen that actually
// consulted the mirror is the one that reports it, and it never overwrites an
// error already on the line.
func (m *model) noteRemoteWarning() {
	if m.remoteWarned || m.status != "" {
		return
	}

	for _, org := range m.orgs {
		err := org.RemoteWarning()
		if err != nil {
			m.status = fmt.Sprintf("mirror unreachable; showing local content only (%v)", err)
			m.remoteWarned = true

			return
		}
	}
}

// abandonLoads walks away from every in-flight screen build: the epoch
// advances past theirs, so each settles into a discarded [loadDoneMsg], and
// the loading indicator clears immediately.
func (m *model) abandonLoads() {
	m.epoch++
	m.loading = 0
	m.spinning = false
}

// teardown closes every stacked screen and returns the command that ends the
// program. A quit taken at the root model -- a ctrl+c, or a screen signaling
// [quitMsg] -- never reaches the screens themselves, so without this an extract
// still running under the top screen would keep its goroutine blocked on a
// channel whose receiver left with the program.
func (m *model) teardown() tea.Cmd {
	for _, s := range m.stack {
		closeScreen(s)
	}

	return tea.Quit
}

// closeScreen runs a screen's teardown hook when it has one. Closing is
// idempotent, so a screen closed on its way off the stack and again by a quit
// that follows costs nothing.
func closeScreen(s screen) {
	if c, ok := s.(closer); ok {
		c.close()
	}
}

// push wraps a screen constructor into a command, surfacing a construction
// problem on the status line instead of descending. A constructor may fetch
// from the remote store, so the command announces the load first; the root
// model counts it, shows a spinner if the build outlives [loadingGrace], and
// runs the build itself. The announcement carries no epoch of its own: the
// root model stamps it as it dispatches the command (see [stampStart]).
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

// stampStart marks the load a screen's command announces with the epoch
// current where the command was dispatched. Bubble Tea runs commands on
// goroutines feeding the same queue as the input reader, so a pop or an
// abandon can be processed between the keypress and the [loadStartMsg] it
// produced; stamping at dispatch is what lets the root model recognize such an
// announcement as belonging to a stack the user has already left. A command
// batching a push alongside other work is wrapped through, since the runtime
// dispatches the batch's commands separately.
func stampStart(cmd tea.Cmd, epoch int) tea.Cmd {
	if cmd == nil {
		return nil
	}

	return func() tea.Msg {
		switch msg := cmd().(type) {
		case loadStartMsg:
			msg.epoch = epoch

			return msg

		case tea.BatchMsg:
			batch := make(tea.BatchMsg, len(msg))
			for i, c := range msg {
				batch[i] = stampStart(c, epoch)
			}

			return batch

		default:
			return msg
		}
	}
}

// stampDone marks a build's settling message with the epoch it launched under,
// so the root model can tell a live outcome from one whose load was abandoned
// mid-flight.
func stampDone(build tea.Cmd, epoch int) tea.Cmd {
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

// quit returns the command that ends the browser, routed through the root
// model so the stack is torn down before the program stops. Screens use it in
// place of tea.Quit.
func quit() tea.Cmd {
	return func() tea.Msg {
		return quitMsg{}
	}
}
