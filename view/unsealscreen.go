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

// maxShownUnsealErrors caps how many per-file failures the progress screen
// lists; the summary still counts them all.
const maxShownUnsealErrors = 5

// unsealCrumb is the breadcrumb segment both unseal screens share.
const unsealCrumb = "unseal"

// unsealPromptScreen asks for the directory an unseal writes into.
//
// Create instances with [newUnsealPromptScreen].
type unsealPromptScreen struct {
	start  func(target string) (screen, error)
	label  string
	input  textinput.Model
	remote bool
}

// newUnsealPromptScreen creates a new [unsealPromptScreen] for the scope named
// label (for example "workspace app"), pre-filled with the default target def.
// When remote is true the prompt notes that evicted bundles will be fetched.
// Confirming calls start with the absolute target to build the next screen.
func newUnsealPromptScreen(label string, remote bool, def string,
	start func(target string) (screen, error),
) *unsealPromptScreen {
	input := textinput.New()
	input.SetValue(def)
	input.SetVirtualCursor(true)

	styles := input.Styles()
	themeInputStyles(&styles)
	input.SetStyles(styles)

	input.Focus()

	return &unsealPromptScreen{label: label, remote: remote, start: start, input: input}
}

// update confirms on enter, cancels on esc, and forwards everything else to
// the text input.
func (s *unsealPromptScreen) update(msg tea.Msg) tea.Cmd {
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
func (s *unsealPromptScreen) view() string {
	var b strings.Builder

	b.WriteString("Unseal " + s.label + " into:\n\n")
	b.WriteString(s.input.View())
	b.WriteString("\n")

	if s.remote {
		b.WriteString("\n" + theme.Muted.Render("evicted bundles will be downloaded from the remote store") + "\n")
	}

	b.WriteString("\n" + styleFooter.Render("enter confirm · esc cancel"))

	return b.String()
}

// crumb names the screen's breadcrumb segment.
func (s *unsealPromptScreen) crumb() string { return unsealCrumb }

// setSize sizes the input to the screen body.
func (s *unsealPromptScreen) setSize(width, _ int) {
	s.input.SetWidth(max(width-4, 10))
}

// unsealProgressScreen drives one unseal run: it starts [runUnseal] when
// pushed and renders its event stream as live progress, then a summary.
//
// Create instances with [newUnsealProgressScreen].
type unsealProgressScreen struct {
	//nolint:containedctx // The run is started from init, a tea.Cmd hook with no context.
	ctx     context.Context
	cancel  context.CancelFunc
	events  chan unsealEvent
	org     *Org
	summary *UnsealSummary
	target  string
	label   string
	current string
	errs    []string
	jobs    []unsealJob
	spin    spinner.Model
	bytes   int64
	done    int
	errored int
}

// newUnsealProgressScreen creates a new [unsealProgressScreen] extracting jobs
// into target for the scope named label. The run's context is a cancelable
// child of the org's browse context, so both the screen's own cancel and an
// external SIGINT stop the loop; work does not start until
// [unsealProgressScreen.init] runs.
func newUnsealProgressScreen(org *Org, target string, jobs []unsealJob, label string) *unsealProgressScreen {
	ctx, cancel := context.WithCancel(org.context())

	spin := spinner.New(spinner.WithSpinner(spinner.Dot))
	spin.Style = theme.Accent

	return &unsealProgressScreen{
		org:    org,
		target: target,
		label:  label,
		jobs:   jobs,
		ctx:    ctx,
		cancel: cancel,
		events: make(chan unsealEvent),
		spin:   spin,
	}
}

// init starts the unseal goroutine and the first listen; the root model runs
// it as the screen is pushed.
func (s *unsealProgressScreen) init() tea.Cmd {
	go runUnseal(s.ctx, s.org, s.target, s.jobs, s.events)

	return tea.Batch(s.spin.Tick, s.listen())
}

// listen blocks for the next unseal event; a closed channel (a canceled run)
// yields no message and ends the listening chain.
func (s *unsealProgressScreen) listen() tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-s.events

		if !ok {
			return nil
		}

		return ev
	}
}

// update folds events into the counters, advances the spinner, and handles
// cancel and navigation keys. Canceling mid-run stops the loop between files;
// an in-flight remote ranged GET runs under the browse context (the
// orgRemote's), not this screen's child, so it finishes or times out on its
// own — only the loop stops promptly. The model-level ctrl+c quit bypasses
// the screen entirely; runUnseal's ctx-guarded sends cover that path.
func (s *unsealProgressScreen) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case unsealEvent:
		if msg.Summary != nil {
			s.summary = msg.Summary

			return nil
		}

		s.current = msg.Path

		if msg.Err != nil {
			s.errored++

			if len(s.errs) < maxShownUnsealErrors {
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
			s.cancel()

			return pop()

		case "q":
			s.cancel()

			return tea.Quit
		}
	}

	return nil
}

// view renders live progress while the run is going and totals once it ends,
// with the first few per-file failures listed either way.
func (s *unsealProgressScreen) view() string {
	var b strings.Builder

	if s.summary == nil {
		b.WriteString(s.spin.View())
		fmt.Fprintf(&b, "unsealing %s · done %d · errored %d · %s\n",
			s.label, s.done, s.errored, theme.HumanBytes(s.bytes))

		if s.current != "" {
			b.WriteString(theme.Muted.Render(firstLine(s.current)) + "\n")
		}
	} else {
		fmt.Fprintf(&b, "%s unsealed %s: %s (%s) into %s\n",
			theme.GlyphOK, s.label,
			countNoun(s.summary.Files, "file", "files"),
			theme.HumanBytes(s.summary.Bytes), s.target)

		if s.summary.Errored > 0 {
			fmt.Fprintf(&b, "%s %s\n", theme.GlyphError,
				countNoun(s.summary.Errored, "file errored", "files errored"))
		}
	}

	for _, e := range s.errs {
		b.WriteString(styleStatusErr.Render(firstLine(e)) + "\n")
	}

	b.WriteString("\n" + styleFooter.Render("esc back · q quit"))

	return b.String()
}

// crumb names the screen's breadcrumb segment.
func (s *unsealProgressScreen) crumb() string { return unsealCrumb }

// setSize is a no-op: the variable-content lines (the current path, the
// per-file errors) are clamped by [firstLine], and the rest is short fixed
// chrome.
func (s *unsealProgressScreen) setSize(int, int) {}
