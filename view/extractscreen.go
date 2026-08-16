package view

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"

	tea "charm.land/bubbletea/v2"

	"go.jacobcolvin.com/hcp_archiver/theme"
)

// maxShownExtractErrors caps how many per-file failures the progress screen
// lists; the summary still counts them all.
const maxShownExtractErrors = 5

// extractCrumb is the breadcrumb segment both extract screens share.
const extractCrumb = "extract"

// extractPromptScreen asks for the directory an extract writes into.
//
// Create instances with [newExtractPromptScreen].
type extractPromptScreen struct {
	start  func(target string) (screen, error)
	label  string
	input  textinput.Model
	remote bool
}

// newExtractPromptScreen creates a new [extractPromptScreen] for the scope named
// label (for example "workspace app"), pre-filled with the default target def.
// When remote is true the prompt notes that evicted bundles will be fetched.
// Confirming calls start with the absolute target to build the next screen.
func newExtractPromptScreen(label string, remote bool, def string,
	start func(target string) (screen, error),
) *extractPromptScreen {
	input := textinput.New()
	input.SetValue(def)
	input.SetVirtualCursor(true)

	styles := input.Styles()
	themeInputStyles(&styles)
	input.SetStyles(styles)

	input.Focus()

	return &extractPromptScreen{label: label, remote: remote, start: start, input: input}
}

// update confirms on enter, cancels on esc, and forwards everything else to
// the text input.
func (s *extractPromptScreen) update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "enter":
			target := strings.TrimSpace(s.input.Value())
			if target == "" {
				return func() tea.Msg { return statusMsg{err: ErrNoTarget} }
			}

			return push(func() (screen, error) {
				abs, err := filepath.Abs(target)
				if err != nil {
					return nil, fmt.Errorf("resolve target %q: %w", target, err)
				}

				return s.start(abs)
			})

		case keyEsc:
			return pop()
		}
	}

	var cmd tea.Cmd

	s.input, cmd = s.input.Update(msg)

	return cmd
}

// view renders the prompt line, the input, and the key hints.
func (s *extractPromptScreen) view() string {
	var b strings.Builder

	b.WriteString("Extract " + s.label + " into:\n\n")
	b.WriteString(s.input.View())
	b.WriteString("\n")

	if s.remote {
		b.WriteString("\n" + theme.Muted.Render("evicted bundles will be downloaded from the remote store") + "\n")
	}

	b.WriteString("\n" + styleFooter.Render("enter confirm · esc cancel"))

	return b.String()
}

// crumb names the screen's breadcrumb segment.
func (s *extractPromptScreen) crumb() string { return extractCrumb }

// setSize sizes the input to the screen body.
func (s *extractPromptScreen) setSize(width, _ int) {
	s.input.SetWidth(max(width-4, 10))
}

// extractProgressScreen drives one extract run: it starts [runExtract] when
// pushed and renders its event stream as live progress, then a summary.
//
// Create instances with [newExtractProgressScreen].
type extractProgressScreen struct {
	//nolint:containedctx // The run is started from init, a tea.Cmd hook with no context.
	ctx     context.Context
	cancel  context.CancelFunc
	events  chan extractEvent
	org     *Org
	summary *ExtractSummary
	target  string
	label   string
	current string
	errs    []string
	jobs    []extractJob
	spin    spinner.Model
	bytes   int64
	done    int
	errored int
}

// newExtractProgressScreen creates a new [extractProgressScreen] extracting jobs
// into target for the scope named label. The run's context is a cancelable
// child of the org's browse context, so [extractProgressScreen.close] and an
// external SIGINT alike stop the loop; work does not start until
// [extractProgressScreen.init] runs.
func newExtractProgressScreen(org *Org, target string, jobs []extractJob, label string) *extractProgressScreen {
	ctx, cancel := context.WithCancel(org.context())

	spin := spinner.New(spinner.WithSpinner(spinner.Dot))
	spin.Style = theme.Accent

	return &extractProgressScreen{
		org:    org,
		target: target,
		label:  label,
		jobs:   jobs,
		ctx:    ctx,
		cancel: cancel,
		events: make(chan extractEvent),
		spin:   spin,
	}
}

// init starts the extract goroutine and the first listen; the root model runs
// it as the screen is pushed.
func (s *extractProgressScreen) init() tea.Cmd {
	go runExtract(s.ctx, s.org, s.target, s.jobs, s.events)

	return tea.Batch(s.spin.Tick, s.listen())
}

// listen blocks for the next extract event; a closed channel (a canceled run)
// yields no message and ends the listening chain.
func (s *extractProgressScreen) listen() tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-s.events

		if !ok {
			return nil
		}

		return ev
	}
}

// update folds events into the counters, advances the spinner, and handles
// navigation keys. Both back keys and q leave the screen, which is what stops
// the run: the root model closes the screen on its way off the stack (see
// [extractProgressScreen.close]), so the cancellation is the same whichever key
// ends the run and whether the screen or the model decided it.
func (s *extractProgressScreen) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case extractEvent:
		if msg.Summary != nil {
			s.summary = msg.Summary

			return nil
		}

		s.current = msg.Path

		if msg.Err != nil {
			s.errored++

			if len(s.errs) < maxShownExtractErrors {
				s.errs = append(s.errs, msg.Path+": "+msg.Err.Error())
			}
		} else {
			s.done++
			s.bytes += msg.Bytes
		}

		return s.listen()

	case spinner.TickMsg:
		if s.summary != nil {
			return nil
		}

		var cmd tea.Cmd

		s.spin, cmd = s.spin.Update(msg)

		return cmd

	case tea.KeyPressMsg:
		switch msg.String() {
		case keyEsc, keyBackspace:
			return pop()

		case "q":
			return quit()
		}
	}

	return nil
}

// close cancels the run: the loop stops between files and [runExtract]'s
// ctx-guarded sends unblock, so the goroutine finishes rather than waiting on
// a channel nothing drains any more. An in-flight remote ranged GET runs under
// the browse context (the orgRemote's), not this screen's child, so it
// finishes or times out on its own; only the loop stops promptly.
//
// The screen is closed by the root model whenever it leaves the stack, which
// covers the paths the screen cannot see: a ctrl+c handled above it, and a
// screen built by a load the user walked away from before it settled.
func (s *extractProgressScreen) close() {
	s.cancel()
}

// view renders live progress while the run is going and totals once it ends,
// with the first few per-file failures listed either way.
func (s *extractProgressScreen) view() string {
	var b strings.Builder

	if s.summary == nil {
		b.WriteString(s.spin.View())
		fmt.Fprintf(&b, "extracting %s · done %d · errored %d · %s\n",
			s.label, s.done, s.errored, theme.HumanBytes(s.bytes))

		if s.current != "" {
			b.WriteString(theme.Muted.Render(firstLine(s.current)) + "\n")
		}
	} else {
		fmt.Fprintf(&b, "%s extracted %s: %s (%s) into %s\n",
			theme.GlyphOK, s.label,
			theme.CountNoun(s.summary.Files, "file", "files"),
			theme.HumanBytes(s.summary.Bytes), s.target)

		if s.summary.Errored > 0 {
			fmt.Fprintf(&b, "%s %s\n", theme.GlyphError,
				theme.CountNoun(s.summary.Errored, "file errored", "files errored"))
		}
	}

	for _, e := range s.errs {
		b.WriteString(styleStatusErr.Render(firstLine(e)) + "\n")
	}

	b.WriteString("\n" + styleFooter.Render("esc back · q quit"))

	return b.String()
}

// crumb names the screen's breadcrumb segment.
func (s *extractProgressScreen) crumb() string { return extractCrumb }

// setSize is a no-op: the variable-content lines (the current path, the
// per-file errors) are clamped by [firstLine], and the rest is short fixed
// chrome.
func (s *extractProgressScreen) setSize(int, int) {}
