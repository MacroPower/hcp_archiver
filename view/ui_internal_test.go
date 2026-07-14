package view

import (
	"strings"
	"testing"

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
