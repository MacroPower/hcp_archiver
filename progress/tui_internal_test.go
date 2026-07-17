package progress

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tea "charm.land/bubbletea/v2"
)

// queueDrain returns a drain func over a mutable queue, mirroring the sink's
// contract: each call returns the queued lines and clears them.
func queueDrain(queue *[]string) func() []string {
	return func() []string {
		lines := *queue
		*queue = nil

		return lines
	}
}

func TestFlushLogs_GatesOnInFlightBatch(t *testing.T) {
	t.Parallel()

	queue := []string{"one"}
	m := newTUIModel(nil, nil, queueDrain(&queue))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	require.NotNil(t, m.flushLogs(), "a queued line yields a batch")

	queue = append(queue, "two")

	assert.Nil(t, m.flushLogs(), "no second batch while the first is unacknowledged")

	// The ack releases the gate and immediately flushes what queued meanwhile.
	_, cmd := m.Update(logFlushedMsg{})
	assert.NotNil(t, cmd, "the ack flushes the lines queued behind the in-flight batch")
	assert.Empty(t, queue, "the release drained the queue")
}

func TestFlushLogs_HoldsUntilSized(t *testing.T) {
	t.Parallel()

	// Before the first size message the width is unknown, so lines cannot be
	// wrapped and must wait in the queue rather than print unwrapped.
	queue := []string{"early"}
	m := newTUIModel(nil, nil, queueDrain(&queue))

	assert.Nil(t, m.flushLogs(), "no batch before the terminal size is known")
	assert.Equal(t, []string{"early"}, queue, "the line stays queued for the sized flush")

	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	assert.NotNil(t, m.flushLogs(), "the first size message releases the held lines")
}

func TestFlushLogs_EmptyQueueYieldsNoCommand(t *testing.T) {
	t.Parallel()

	queue := []string{}
	m := newTUIModel(nil, nil, queueDrain(&queue))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	assert.Nil(t, m.flushLogs())
	assert.False(t, m.logsInFlight, "an empty drain arms no gate")
}

func TestLogBatch_HardwrapsToTerminalWidth(t *testing.T) {
	t.Parallel()

	// A line wider than the terminal must be pre-wrapped: the inline renderer
	// estimates a printed line's height from its width, and an over-wide line
	// breaks that estimate and shifts the panel's origin.
	queue := []string{strings.Repeat("x", 25)}
	m := newTUIModel(nil, nil, queueDrain(&queue))
	m.Update(tea.WindowSizeMsg{Width: 10, Height: 24})

	batch := m.logBatch()

	for line := range strings.SplitSeq(batch, "\n") {
		assert.LessOrEqual(t, len(line), 10, "every physical line fits the terminal")
	}

	assert.Equal(t, strings.Repeat("x", 25), strings.ReplaceAll(batch, "\n", ""),
		"wrapping loses no content")
}

func TestQuit_SequencesFinalFlushBeforeQuit(t *testing.T) {
	t.Parallel()

	queue := []string{"last words"}
	m := newTUIModel(nil, nil, queueDrain(&queue))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	_, cmd := m.Update(quitRequestMsg{})
	require.NotNil(t, cmd)
	assert.True(t, m.quitting, "the model quits after the flush")
	assert.Empty(t, queue, "the quit path drained the queue")
}
