package view

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/spinner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tea "charm.land/bubbletea/v2"
)

// writeBytesJob builds an [extractJob] write function serving fixed content, the
// shape the screen tests need without an archive behind it.
func writeBytesJob(content string) func(context.Context, string, io.Writer) (int64, error) {
	return func(_ context.Context, _ string, w io.Writer) (int64, error) {
		n, err := w.Write([]byte(content))

		return int64(n), err
	}
}

// initStub is a screen that records whether the root model ran its init hook.
type initStub struct {
	stubScreen
}

// initRanMsg marks that the stub's init command was executed.
type initRanMsg struct{}

func (s *initStub) init() tea.Cmd {
	return func() tea.Msg { return initRanMsg{} }
}

// pressOn sends one key to a screen and returns the resulting command.
func pressOn(s screen, k tea.Key) tea.Cmd {
	return s.update(tea.KeyPressMsg(k))
}

func TestListScreenExtractKeyPushesPrompt(t *testing.T) {
	t.Parallel()

	// The hint comes from the rows via newListScreen; there is no opt-in
	// flag for a caller to forget.
	prompt := stubScreen{name: "extract"}
	s := newListScreen("test", []item{{
		title:   "app",
		desc:    "extractable row",
		extract: func() (screen, error) { return prompt, nil },
	}})
	s.setSize(80, 24)

	cmd := pressOn(s, tea.Key{Code: 'e', Text: "e"})
	require.NotNil(t, cmd)

	msg, ok := execPush(t, cmd).(pushMsg)
	require.True(t, ok, "e on an extractable row descends into the prompt")
	assert.Equal(t, prompt, msg.s)
}

func TestListScreenExtractKeyIsTypableWhileFiltering(t *testing.T) {
	t.Parallel()

	s := newListScreen("test", []item{{
		title:   "app",
		desc:    "extractable row",
		extract: func() (screen, error) { return stubScreen{}, nil },
	}})
	s.setSize(80, 24)

	pressOn(s, tea.Key{Code: '/', Text: "/"})
	require.Equal(t, list.Filtering, s.list.FilterState())

	pressOn(s, tea.Key{Code: 'e', Text: "e"})

	assert.Equal(t, list.Filtering, s.list.FilterState(), "the key stays with the filter prompt")
	assert.Equal(t, "e", s.list.FilterInput.Value(), "e types into the filter, not the extract action")
}

func TestListScreenExtractKeyInertWithoutExtractRows(t *testing.T) {
	t.Parallel()

	// A screen without extractable rows binds nothing to e; the intercept
	// must not push a prompt there, and the stock paging keys stay whole.
	rows := make([]item, 30)
	for i := range rows {
		rows[i] = item{title: "row", desc: "plain"}
	}

	s := newListScreen("test", rows)
	s.setSize(40, 10)

	pressOn(s, tea.Key{Code: tea.KeyRight})
	require.Positive(t, s.list.Paginator.Page, "the fixture spans multiple pages")

	page := s.list.Paginator.Page
	cmd := pressOn(s, tea.Key{Code: 'e', Text: "e"})
	assert.Nil(t, cmd, "e pushes nothing on screens without extract rows")
	assert.Equal(t, page, s.list.Paginator.Page, "e does not page")

	pressOn(s, tea.Key{Code: 'u', Text: "u"})
	assert.Equal(t, 0, s.list.Paginator.Page, "u keeps its stock prev-page binding")
}

func TestExtractPromptScreen(t *testing.T) {
	t.Parallel()

	progress := stubScreen{name: "progress"}

	newPrompt := func(started *string) *extractPromptScreen {
		return newExtractPromptScreen("workspace app", false, "/tmp/default",
			func(target string) (screen, error) {
				if started != nil {
					*started = target
				}

				return progress, nil
			})
	}

	t.Run("enter confirms with the entered target", func(t *testing.T) {
		t.Parallel()

		var started string

		s := newPrompt(&started)

		cmd := pressOn(s, tea.Key{Code: tea.KeyEnter})
		require.NotNil(t, cmd)

		msg, ok := execPush(t, cmd).(pushMsg)
		require.True(t, ok)
		assert.Equal(t, progress, msg.s)
		assert.Equal(t, "/tmp/default", started, "the pre-filled default is confirmed as-is")
	})

	t.Run("empty target surfaces ErrNoTarget", func(t *testing.T) {
		t.Parallel()

		s := newPrompt(nil)
		s.input.SetValue("  ")

		cmd := pressOn(s, tea.Key{Code: tea.KeyEnter})
		require.NotNil(t, cmd)

		msg, ok := cmd().(statusMsg)
		require.True(t, ok)
		require.ErrorIs(t, msg.err, ErrNoTarget)
	})

	t.Run("esc cancels", func(t *testing.T) {
		t.Parallel()

		s := newPrompt(nil)

		cmd := pressOn(s, tea.Key{Code: tea.KeyEscape})
		require.NotNil(t, cmd)

		_, ok := cmd().(popMsg)
		assert.True(t, ok)
	})

	t.Run("remote note renders only for remote archives", func(t *testing.T) {
		t.Parallel()

		local := newExtractPromptScreen("workspace app", false, "d", nil)
		assert.NotContains(t, local.view(), "remote store")

		remote := newExtractPromptScreen("workspace app", true, "d", nil)
		assert.Contains(t, remote.view(), "downloaded from the remote store")
	})
}

func TestExtractProgressScreenFoldsEvents(t *testing.T) {
	t.Parallel()

	org := &Org{Name: "my-org"}
	s := newExtractProgressScreen(org, t.TempDir(), nil, "workspace app")

	cmd := s.update(extractEvent{Path: "a.json", Bytes: 5})
	require.NotNil(t, cmd, "a per-file event re-arms the listener")

	cmd = s.update(extractEvent{Path: "b.json", Err: assert.AnError})
	require.NotNil(t, cmd)

	assert.Equal(t, 1, s.done)
	assert.Equal(t, 1, s.errored)
	assert.EqualValues(t, 5, s.bytes)

	view := s.view()
	assert.Contains(t, view, "done 1")
	assert.Contains(t, view, "errored 1")
	assert.Contains(t, view, "b.json", "the failed file lists")

	cmd = s.update(extractEvent{Summary: &ExtractSummary{Files: 1, Errored: 1, Bytes: 5}})
	assert.Nil(t, cmd, "the terminal event ends the listening chain")

	view = s.view()
	assert.Contains(t, view, "extracted workspace app")
	assert.Contains(t, view, "1 file errored")
}

func TestExtractProgressScreenInitRunsToSummary(t *testing.T) {
	t.Parallel()

	// Drive the real init: the goroutine, the spinner tick, and the listen
	// chain all start from its batch, so a regression there passes no other
	// test. Executing every returned command until the summary folds in
	// proves the loop closes end to end.
	org := &Org{Name: "my-org"}
	target := t.TempDir()

	jobs := []extractJob{{rel: "a.json", write: writeBytesJob("data")}}
	s := newExtractProgressScreen(org, target, jobs, "workspace app")

	pending := []tea.Cmd{s.init()}
	sawTick := false

	for i := 0; s.summary == nil; i++ {
		require.Less(t, i, 1000, "the run must reach its summary")
		require.NotEmpty(t, pending, "commands must keep flowing until the summary lands")

		cmd := pending[0]
		pending = pending[1:]

		if cmd == nil {
			continue
		}

		switch msg := cmd().(type) {
		case tea.BatchMsg:
			pending = append(pending, msg...)
		default:
			if _, ok := msg.(spinner.TickMsg); ok {
				sawTick = true
			}

			if next := s.update(msg); next != nil {
				pending = append(pending, next)
			}
		}
	}

	assert.Equal(t, 1, s.done)
	assert.Equal(t, 0, s.errored)
	assert.Equal(t, &ExtractSummary{Files: 1, Bytes: 4}, s.summary)
	assert.True(t, sawTick, "init's batch carries the spinner tick")
	assert.FileExists(t, filepath.Join(target, "my-org", "a.json"))
}

func TestExtractProgressScreenEscCancelsMidRun(t *testing.T) {
	t.Parallel()

	org := &Org{Name: "my-org"}
	s := newExtractProgressScreen(org, t.TempDir(), nil, "workspace app")
	m := newTestModel(stubScreen{name: "root"}, s)

	cmd := pressOn(s, tea.Key{Code: tea.KeyEscape})
	require.NotNil(t, cmd)

	msg, ok := cmd().(popMsg)
	require.True(t, ok, "esc pops the screen")

	m.Update(msg)

	require.Len(t, m.stack, 1)
	require.Error(t, s.ctx.Err(), "leaving the screen cancels the run context")
}

func TestExtractProgressScreenQuitKeyRoutesThroughTheModel(t *testing.T) {
	t.Parallel()

	// The q key must not end the program from the screen itself: the runtime
	// would swallow the quit and the run's context would never be canceled.
	org := &Org{Name: "my-org"}
	s := newExtractProgressScreen(org, t.TempDir(), nil, "workspace app")
	m := newTestModel(stubScreen{name: "root"}, s)

	cmd := pressOn(s, tea.Key{Code: 'q', Text: "q"})
	require.NotNil(t, cmd)

	msg, ok := cmd().(quitMsg)
	require.True(t, ok, "q asks the root model to quit")

	_, cmd = m.Update(msg)
	require.NotNil(t, cmd)
	assert.IsType(t, tea.QuitMsg{}, cmd())
	require.Error(t, s.ctx.Err(), "the teardown cancels the run context")
}

func TestModelQuitStopsAnActiveExtract(t *testing.T) {
	t.Parallel()

	// The interrupt lands mid-run with nothing draining the events channel,
	// exactly as it does once the program stops reading: without the model's
	// teardown, runExtract blocks on its next send forever and the remaining
	// files are silently never extracted.
	org := &Org{Name: "my-org"}
	jobs := []extractJob{
		{rel: "a.json", write: writeBytesJob("a")},
		{rel: "b.json", write: writeBytesJob("b")},
	}

	s := newExtractProgressScreen(org, t.TempDir(), jobs, "workspace app")
	m := newTestModel(stubScreen{name: "root"}, s)

	s.init()

	_, cmd := m.Update(ctrlC())
	require.NotNil(t, cmd)
	assert.IsType(t, tea.QuitMsg{}, cmd())
	require.Error(t, s.ctx.Err(), "the interrupt cancels the run context")

	// The goroutine closing its channel is what proves it is not stranded on a
	// send no receiver will ever take.
	deadline := time.After(10 * time.Second)

	for {
		select {
		case _, ok := <-s.events:
			if !ok {
				return
			}

		case <-deadline:
			t.Fatal("the extract goroutine outlived the quit that stopped its run")
		}
	}
}

func TestModelAbandonedExtractScreenIsCanceled(t *testing.T) {
	t.Parallel()

	// The screen's context is a child of the browse context from construction,
	// before init ever runs, so a build the user walked away from leaks it into
	// the browse context's lifetime unless the drop closes the screen.
	org := &Org{Name: "my-org"}
	built := newExtractProgressScreen(org, t.TempDir(), nil, "workspace app")
	m := newTestModel(stubScreen{name: "root"})

	_, cmd := m.Update(announce(t, m, push(func() (screen, error) { return built, nil })))
	staleDone := settleLoad(t, cmd)

	m.abandonLoads()
	m.Update(staleDone)

	require.Len(t, m.stack, 1, "the abandoned screen never pushes")
	assert.Error(t, built.ctx.Err(), "the dropped screen's run context is canceled")
}

func TestRunExtractCanceledWithoutReceiverDoesNotStrand(t *testing.T) {
	t.Parallel()

	org := &Org{Name: "my-org"}
	ctx, cancel := context.WithCancel(t.Context())
	events := make(chan extractEvent)

	jobs := []extractJob{
		{rel: "a.json", write: writeBytesJob("a")},
		{rel: "b.json", write: writeBytesJob("b")},
	}

	go runExtract(ctx, org, t.TempDir(), jobs, events)

	// Cancel while nothing drains the channel: the ctx-guarded sends must
	// unblock the goroutine so it closes events rather than leaking.
	cancel()

	deadline := time.After(10 * time.Second)

	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}

		case <-deadline:
			t.Fatal("runExtract never closed its channel after cancellation")
		}
	}
}

func TestModelPushRunsInitializer(t *testing.T) {
	t.Parallel()

	m := &model{stack: []screen{stubScreen{name: "root"}}}

	_, cmd := m.Update(pushMsg{s: &initStub{stubScreen{name: "async"}}})
	require.NotNil(t, cmd, "pushing an initializer screen returns its init command")

	_, ok := cmd().(initRanMsg)
	assert.True(t, ok)
}

func TestExtractProgressScreenViewTruncatesCurrentPath(t *testing.T) {
	t.Parallel()

	org := &Org{Name: "my-org"}
	s := newExtractProgressScreen(org, t.TempDir(), nil, "workspace app")

	long := strings.Repeat("x", 200)
	s.update(extractEvent{Path: long, Bytes: 1})

	assert.NotContains(t, s.view(), long, "the current path is clamped to a list-friendly width")
}
