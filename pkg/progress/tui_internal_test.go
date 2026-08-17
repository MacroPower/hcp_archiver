package progress

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tea "charm.land/bubbletea/v2"
)

// activeSink returns a [LogWriter] in its queueing state carrying the given
// lines, so the model tests exercise the production feed rather than a
// hand-rolled stand-in.
func activeSink(t *testing.T, lines ...string) *LogWriter {
	t.Helper()

	w := NewLogWriter(io.Discard)
	w.Activate()

	for _, line := range lines {
		_, err := io.WriteString(w, line+"\n")
		require.NoError(t, err)
	}

	return w
}

// peekedLines returns the sink's queued lines without consuming them.
func peekedLines(w *LogWriter) []string {
	lines, _ := w.Peek()

	return lines
}

func TestFlushLogs_GatesOnInFlightBatch(t *testing.T) {
	t.Parallel()

	w := activeSink(t, "one")
	m := newTUIModel(nil, nil, w)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	require.NotNil(t, m.flushLogs(), "a queued line yields a batch")

	_, err := io.WriteString(w, "two\n")
	require.NoError(t, err)

	assert.Nil(t, m.flushLogs(), "no second batch while the first is unacknowledged")

	// The ack commits the in-flight batch and immediately flushes what queued
	// meanwhile.
	_, cmd := m.Update(logFlushedMsg{})
	assert.NotNil(t, cmd, "the ack flushes the lines queued behind the in-flight batch")

	// Both batches are now peeked-but-uncommitted ("two") or committed ("one"):
	// acking the second empties the sink entirely.
	m.Update(logFlushedMsg{})
	assert.Empty(t, peekedLines(w), "acked batches are committed out of the sink")
}

func TestFlushLogs_UncommittedLinesSurviveForFallback(t *testing.T) {
	t.Parallel()

	// A batch peeked but never acked (the program died mid-print) must stay in
	// the sink, where Deactivate's fallback flush recovers it.
	buf := &strings.Builder{}
	w := NewLogWriter(buf)
	w.Activate()

	_, err := io.WriteString(w, "in flight\n")
	require.NoError(t, err)

	m := newTUIModel(nil, nil, w)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	require.NotNil(t, m.flushLogs(), "the batch is emitted")

	w.Deactivate()
	assert.Equal(t, "in flight\n", buf.String(),
		"the unacked batch reaches the fallback instead of vanishing")
}

func TestFlushLogs_HoldsUntilSized(t *testing.T) {
	t.Parallel()

	// Before the first size message the width is unknown, so lines cannot be
	// wrapped and must wait in the queue rather than print unwrapped.
	w := activeSink(t, "early")
	m := newTUIModel(nil, nil, w)

	assert.Nil(t, m.flushLogs(), "no batch before the terminal size is known")
	assert.Equal(t, []string{"early"}, peekedLines(w), "the line stays queued for the sized flush")

	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	assert.NotNil(t, m.flushLogs(), "the first size message releases the held lines")
}

func TestFlushLogs_EmptyQueueYieldsNoCommand(t *testing.T) {
	t.Parallel()

	w := activeSink(t)
	m := newTUIModel(nil, nil, w)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	assert.Nil(t, m.flushLogs())
	assert.False(t, m.logsInFlight, "an empty peek arms no gate")
}

func TestFlushLogs_HardwrapsToTerminalWidth(t *testing.T) {
	t.Parallel()

	// A line wider than the terminal must be pre-wrapped: the inline renderer
	// estimates a printed line's height from its width, and an over-wide line
	// breaks that estimate and shifts the panel's origin. The wrap must also
	// not corrupt what stays in the sink for a fallback flush.
	wide := strings.Repeat("x", 25)
	w := activeSink(t, wide)
	m := newTUIModel(nil, nil, w)
	m.Update(tea.WindowSizeMsg{Width: 10, Height: 24})

	require.NotNil(t, m.flushLogs())

	assert.Equal(t, []string{wide}, peekedLines(w),
		"wrapping mutates the peeked copy, not the sink's queue")
}

func TestQuit_SequencesFinalFlushBeforeQuit(t *testing.T) {
	t.Parallel()

	w := activeSink(t, "last words")
	m := newTUIModel(nil, nil, w)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	_, cmd := m.Update(quitRequestMsg{})
	require.NotNil(t, cmd)
	assert.True(t, m.quitting, "the model quits after the flush")
	assert.True(t, m.logsInFlight, "the final batch rides ahead of the quit")
}

func TestFeedGuard_RevocationSilencesAStaleModel(t *testing.T) {
	t.Parallel()

	// An abandoned program still holds its feed; once revoked its peeks see
	// nothing and its commits consume nothing, so a successor panel owns the
	// sink alone.
	w := activeSink(t, "next run's line")
	g := &feedGuard{sink: w}

	lines, cursor := g.Peek()
	require.Equal(t, []string{"next run's line"}, lines)

	g.revoked.Store(true)

	stolen, _ := g.Peek()
	assert.Empty(t, stolen, "a revoked feed peeks nothing")

	g.Commit(cursor)
	assert.Equal(t, []string{"next run's line"}, peekedLines(w),
		"a revoked commit consumes nothing from the sink")
}
