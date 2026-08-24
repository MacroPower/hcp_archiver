package progress

import (
	"fmt"
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

// sizeAndPaint delivers a size message at the conventional 24-row height and
// renders one frame, the two gates a flush waits behind: an unsized model
// cannot wrap, and an unpainted one has no frame for the chunk budget to
// measure against. Bubble Tea paints after every Update, so a sized
// production model is always painted too.
func sizeAndPaint(m *tuiModel, width int) {
	m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
	m.View()
}

func TestFlushLogs_GatesOnInFlightBatch(t *testing.T) {
	t.Parallel()

	w := activeSink(t, "one")
	m := newTUIModel(nil, nil, w)
	sizeAndPaint(m, 80)

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
	sizeAndPaint(m, 80)

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

	sizeAndPaint(m, 80)
	assert.NotNil(t, m.flushLogs(), "sizing and painting the panel releases the held lines")
}

func TestFlushLogs_HoldsUntilFirstPaint(t *testing.T) {
	t.Parallel()

	// A sized but never-painted model has no frame, so the chunk budget the
	// flush cuts blocks against does not exist yet; the queue holds until the
	// first paint establishes it.
	w := activeSink(t, "early")
	m := newTUIModel(nil, nil, w)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	assert.Nil(t, m.flushLogs(), "no batch before the panel has painted")
	assert.Equal(t, []string{"early"}, peekedLines(w), "the line stays queued for the painted flush")

	m.View()
	assert.NotNil(t, m.flushLogs(), "the first paint releases the held lines")
}

func TestFlushLogs_EmptyQueueYieldsNoCommand(t *testing.T) {
	t.Parallel()

	w := activeSink(t)
	m := newTUIModel(nil, nil, w)
	sizeAndPaint(m, 80)

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
	sizeAndPaint(m, 10)

	require.NotNil(t, m.flushLogs())

	assert.Equal(t, []string{wide}, peekedLines(w),
		"wrapping mutates the peeked copy, not the sink's queue")
}

func TestQuit_SequencesFinalFlushBeforeQuit(t *testing.T) {
	t.Parallel()

	w := activeSink(t, "last words")
	m := newTUIModel(nil, nil, w)
	sizeAndPaint(m, 80)

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

func TestLogChunks(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		lines  []string
		width  int
		budget int
		want   []string
	}{
		"short batch fits one chunk": {
			lines: []string{"a", "b"}, width: 80, budget: 10,
			want: []string{"a\nb"},
		},
		"rows group into budget-sized chunks": {
			lines: []string{"a", "b", "c", "d", "e"}, width: 80, budget: 2,
			want: []string{"a\nb", "c\nd", "e"},
		},
		"wide line counts its wrapped rows": {
			lines: []string{strings.Repeat("x", 10)}, width: 4, budget: 2,
			want: []string{"xxxx\nxxxx", "xx"},
		},
		"line taller than the budget splits mid-line": {
			lines: []string{strings.Repeat("x", 12)}, width: 4, budget: 2,
			want: []string{"xxxx\nxxxx", "xxxx"},
		},
		"tabs expand before wrapping": {
			lines: []string{"a\tb"}, width: 80, budget: 10,
			want: []string{"a    b"},
		},
		"tab expansion can force a wrap": {
			lines: []string{"\tabc"}, width: 6, budget: 10,
			want: []string{"    ab\nc"},
		},
		"budget one yields one row per chunk": {
			lines: []string{"a", "b"}, width: 80, budget: 1,
			want: []string{"a", "b"},
		},
		"non-positive budget clamps to one row per chunk": {
			lines: []string{"a", "b"}, width: 80, budget: 0,
			want: []string{"a", "b"},
		},
		"blank line occupies a row": {
			lines: []string{"", "x"}, width: 80, budget: 1,
			want: []string{"", "x"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, logChunks(tc.lines, tc.width, tc.budget))
		})
	}
}

func TestLogRowBudget(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		height int
		frame  int
		want   int
	}{
		"typical terminal":                       {height: 24, frame: 8, want: 15},
		"unknown height falls back to 24":        {height: 0, frame: 8, want: 15},
		"negative height falls back to 24":       {height: -1, frame: 8, want: 15},
		"frame filling the screen floors at one": {height: 24, frame: 24, want: 1},
		"frame past the screen floors at one":    {height: 10, frame: 12, want: 1},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m := &tuiModel{height: tc.height, frame: tc.frame}
			assert.Equal(t, tc.want, m.logRowBudget())
		})
	}
}

func TestFlushLogs_ChunksShareOneAck(t *testing.T) {
	t.Parallel()

	// A burst far taller than the screen flushes as many chunks, but the
	// batch stays atomic to the feed: one gate while it flies, one ack, one
	// whole-batch commit.
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}

	w := activeSink(t, lines...)
	m := newTUIModel(nil, nil, w)
	sizeAndPaint(m, 80)

	require.NotNil(t, m.flushLogs(), "a queued burst yields a batch")
	assert.Nil(t, m.flushLogs(), "the chunked batch holds the same single in-flight gate")

	m.Update(logFlushedMsg{})
	assert.Empty(t, peekedLines(w), "one ack commits the whole chunked batch")
}

func TestComposeFrame_HoldsWhileLogsInFlight(t *testing.T) {
	t.Parallel()

	// The in-flight batch's chunks were cut against the frame as it stood, so
	// taller natural content must not ratchet the frame up mid-batch; the
	// composed frame keeps its held height, tail-truncated, until the ack
	// clears the flag.
	m := &tuiModel{frame: 2, logsInFlight: true}

	body := []string{"one", "two", "three"}
	footer := []string{"counts", "meta"}

	lines := m.composeFrame(body, footer)
	assert.Equal(t, 2, m.frame, "an in-flight batch holds the ratchet")
	assert.Equal(t, []string{"counts", "meta"}, lines,
		"the composed frame keeps its held height, topmost rows truncated")

	m.logsInFlight = false

	lines = m.composeFrame(body, footer)
	assert.Equal(t, 5, m.frame, "the cleared gate lets the ratchet resume")
	assert.Equal(t, []string{"one", "two", "three", "counts", "meta"}, lines)
}
