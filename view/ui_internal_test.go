package view

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tea "charm.land/bubbletea/v2"
)

// stubScreen is a minimal [screen] for exercising the root model's chrome
// layout: a fixed crumb and a body of known content.
type stubScreen struct {
	name string
	body string
}

func (s stubScreen) update(tea.Msg) tea.Cmd { return nil }
func (s stubScreen) view() string           { return s.body }
func (s stubScreen) crumb() string          { return s.name }
func (s stubScreen) setSize(int, int)       {}

// recordScreen is a [stubScreen] that records the messages routed to it, for
// asserting what the root model forwards.
type recordScreen struct {
	stubScreen
	msgs []tea.Msg
}

func (s *recordScreen) update(msg tea.Msg) tea.Cmd {
	s.msgs = append(s.msgs, msg)

	return nil
}

// execPush drives a push command the way the root model would: it unwraps the
// load announcement, runs the build, and returns the settled outcome message.
func execPush(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()

	start, ok := cmd().(loadStartMsg)
	require.True(t, ok, "a push command announces its load first")

	done, ok := start.build().(loadDoneMsg)
	require.True(t, ok, "the build settles the load")

	return done.msg
}

// settleLoad drives the command the root model returned for a load
// announcement: it runs the epoch-stamped build (skipping the grace timer
// batched alongside it) and returns the settling message the runtime would
// deliver. The caller decides when to feed it back into Update, so a test can
// abandon the load first.
func settleLoad(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	require.NotNil(t, cmd)

	msg := cmd()

	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		require.IsType(t, loadDoneMsg{}, msg)

		return msg
	}

	for _, c := range batch {
		if done, ok := c().(loadDoneMsg); ok {
			return done
		}
	}

	t.Fatal("the announcement's batch carries no build")

	return nil
}

// announce unwraps a push command's load announcement.
func announce(t *testing.T, cmd tea.Cmd) loadStartMsg {
	t.Helper()

	start, ok := cmd().(loadStartMsg)
	require.True(t, ok, "a push command announces its load first")

	return start
}

// newTestModel builds a root model over a stub stack with a real spinner, the
// way newModel would, without needing an archive on disk.
func newTestModel(stack ...screen) *model {
	return &model{stack: stack, spin: spinner.New(spinner.WithSpinner(spinner.Dot))}
}

func TestViewClampsChromeToOneRowEach(t *testing.T) {
	t.Parallel()

	const (
		width = 20
		body  = "BODY"
	)

	// A deep trail whose joined breadcrumb is far wider than the terminal, and a
	// status carrying a literal newline: unclamped, the breadcrumb soft-wraps and
	// the status spans two rows, so the chrome exceeds chromeLines and the body's
	// bottom is clipped off the terminal.
	m := &model{
		stack: []screen{
			stubScreen{name: "organization-alpha"},
			stubScreen{name: "project-beta"},
			stubScreen{name: "workspace-gamma"},
			stubScreen{name: "run-delta", body: body},
		},
		status: "the first line of a long error message\nand a second line",
		width:  width,
		height: 24,
	}

	lines := strings.Split(m.View().Content, "\n")

	require.GreaterOrEqual(t, len(lines), 3, "breadcrumb, status, then body")
	assert.LessOrEqual(t, lipgloss.Width(lines[0]), width,
		"the breadcrumb is clamped to one physical row so it cannot soft-wrap")
	assert.LessOrEqual(t, lipgloss.Width(lines[1]), width, "the status is width-clamped")
	assert.Equal(t, body, lines[2],
		"the body sits exactly chromeLines below the top; the multi-line status is clamped to one row")
}

func TestViewUnsizedBreadcrumbStillRenders(t *testing.T) {
	t.Parallel()

	// Before the first WindowSizeMsg the width is zero; MaxWidth(0) must be a
	// no-op rather than truncating the breadcrumb to nothing.
	m := &model{
		stack: []screen{stubScreen{name: "organization-alpha", body: "BODY"}},
	}

	lines := strings.Split(m.View().Content, "\n")

	assert.NotEmpty(t, lines[0], "an unsized breadcrumb renders in full")
}

func TestModelShowsSpinnerWhileLoadOutlivesGrace(t *testing.T) {
	t.Parallel()

	m := newTestModel(stubScreen{name: "root", body: "BODY"})

	built := stubScreen{name: "child"}
	cmd := push(func() (screen, error) { return built, nil })

	start, ok := cmd().(loadStartMsg)
	require.True(t, ok, "push announces the load before building")

	_, startCmd := m.Update(start)
	require.NotNil(t, startCmd, "the announcement carries the build and the grace timer")
	require.Equal(t, 1, m.loading)
	assert.NotContains(t, m.View().Content, "loading", "the indicator waits out the grace period")

	// The grace elapses while the build is still in flight: the status row
	// becomes a spinner.
	_, tick := m.Update(loadGraceMsg{})
	require.NotNil(t, tick, "an outlived grace starts the spinner ticking")
	assert.True(t, m.spinning)
	assert.Contains(t, m.View().Content, "loading")

	// The build settles: the screen pushes and the indicator clears.
	done, ok := start.build().(loadDoneMsg)
	require.True(t, ok)

	m.Update(done)

	assert.Equal(t, 0, m.loading)
	assert.False(t, m.spinning)
	assert.Len(t, m.stack, 2, "the built screen is pushed")
	assert.NotContains(t, m.View().Content, "loading")
}

func TestModelGraceAfterSettledLoadStaysQuiet(t *testing.T) {
	t.Parallel()

	m := newTestModel(stubScreen{name: "root"})

	cmd := push(func() (screen, error) { return stubScreen{name: "child"}, nil })

	start, ok := cmd().(loadStartMsg)
	require.True(t, ok)

	// The build settles before its grace timer fires, the ordinary local
	// navigation case: the late grace must not strand a spinner.
	m.Update(start)
	m.Update(start.build())

	_, tick := m.Update(loadGraceMsg{})
	assert.Nil(t, tick, "a grace firing after the build settles starts nothing")
	assert.False(t, m.spinning)
	assert.NotContains(t, m.View().Content, "loading")
}

func TestModelStaleGraceStartsNoSecondSpinnerChain(t *testing.T) {
	t.Parallel()

	// Two navigations inside the grace window: the first load settles, the
	// second arms its own timer, and the first load's timer is still pending.
	// Both fire while the second build is in flight, and the stale one must
	// not start a chain of its own beside the one already ticking.
	m := newTestModel(stubScreen{name: "root"})

	first := push(func() (screen, error) { return stubScreen{name: "first"}, nil })
	firstStart := announce(t, first)

	m.Update(firstStart)
	m.Update(firstStart.build())

	second := push(func() (screen, error) { return stubScreen{name: "second"}, nil })

	m.Update(announce(t, second))

	_, tick := m.Update(loadGraceMsg{})
	require.NotNil(t, tick, "the first grace to arrive starts the chain")
	require.True(t, m.spinning)

	_, stale := m.Update(loadGraceMsg{})
	assert.Nil(t, stale, "a second grace rides the running chain rather than forking one")
	assert.True(t, m.spinning, "the indicator stays up for the load still in flight")
}

func TestModelLoadErrorSurfacesOnStatusLine(t *testing.T) {
	t.Parallel()

	m := newTestModel(stubScreen{name: "root"})

	cmd := push(func() (screen, error) { return nil, assert.AnError })

	start, ok := cmd().(loadStartMsg)
	require.True(t, ok)

	m.Update(start)
	m.Update(loadGraceMsg{})
	require.True(t, m.spinning, "the fixture reaches the spinning state before failing")

	m.Update(start.build())

	assert.False(t, m.spinning, "a failed build clears the indicator")
	assert.Len(t, m.stack, 1, "a failed build pushes nothing")
	assert.Contains(t, m.status, assert.AnError.Error())
	assert.Contains(t, m.View().Content, assert.AnError.Error(),
		"the error takes the status row the spinner occupied")
}

func TestModelEscAbandonsInFlightLoad(t *testing.T) {
	t.Parallel()

	rs := &recordScreen{stubScreen: stubScreen{name: "root"}}
	m := newTestModel(rs)

	stale := push(func() (screen, error) { return stubScreen{name: "stale"}, nil })

	_, cmd := m.Update(announce(t, stale))
	staleDone := settleLoad(t, cmd)

	m.Update(loadGraceMsg{})
	require.True(t, m.spinning, "the fixture reaches the spinning state before the esc")

	// Esc abandons the load: the indicator clears at once and the key never
	// reaches the screen below, so nothing pops.
	_, cmd = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	assert.Nil(t, cmd)
	assert.False(t, m.spinning)
	assert.Equal(t, 0, m.loading)
	assert.Empty(t, rs.msgs, "the consumed esc is not forwarded")

	// A second esc, with nothing in flight, reaches the screen as usual.
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	assert.Len(t, rs.msgs, 1)

	// The abandoned build settles into the void: no push, no indicator.
	m.Update(staleDone)
	assert.Len(t, m.stack, 1, "the abandoned screen never pushes")
	assert.False(t, m.spinning)

	// A load after the abandonment settles normally.
	fresh := push(func() (screen, error) { return stubScreen{name: "fresh"}, nil })

	_, cmd = m.Update(announce(t, fresh))
	m.Update(settleLoad(t, cmd))

	assert.Len(t, m.stack, 2, "a fresh load still pushes")
}

func TestModelPopAbandonsInFlightLoad(t *testing.T) {
	t.Parallel()

	m := newTestModel(stubScreen{name: "root"}, stubScreen{name: "child"})

	stale := push(func() (screen, error) { return stubScreen{name: "stale"}, nil })

	_, cmd := m.Update(announce(t, stale))
	staleDone := settleLoad(t, cmd)

	// The child pops (a backspace routed to it, say) while its load is still
	// in flight.
	m.Update(popMsg{})
	require.Len(t, m.stack, 1)
	assert.Equal(t, 0, m.loading)

	// The load settles after the pop: its screen belongs to a context the
	// user already left, so it never pushes onto the parent.
	m.Update(staleDone)
	assert.Len(t, m.stack, 1)
}

func TestModelRoutesSpinnerTicksByID(t *testing.T) {
	t.Parallel()

	rs := &recordScreen{stubScreen: stubScreen{name: "root"}}
	m := newTestModel(rs)
	m.spinning = true

	// The root spinner's own tick is consumed: it advances the indicator and
	// never reaches the screen below.
	_, cmd := m.Update(spinner.TickMsg{ID: m.spin.ID()})
	assert.NotNil(t, cmd, "a consumed tick re-arms the chain")
	assert.Empty(t, rs.msgs)

	// Another spinner's tick (an unseal run's) belongs to that screen.
	m.Update(spinner.TickMsg{ID: m.spin.ID() + 1})
	assert.Len(t, rs.msgs, 1, "a foreign tick is forwarded to the active screen")
}
