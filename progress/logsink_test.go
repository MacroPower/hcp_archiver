package progress_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/progress"
)

func TestLogWriter_Fallback(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	w := progress.NewLogWriter(buf)

	n, err := w.Write([]byte("first line\n"))
	require.NoError(t, err)
	assert.Equal(t, len("first line\n"), n)

	_, err = w.Write([]byte("second line\n"))
	require.NoError(t, err)

	assert.Equal(t, "first line\nsecond line\n", buf.String())
}

func TestLogWriter_ActiveQueuesForPeek(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	w := progress.NewLogWriter(buf)

	w.Activate()

	n, err := w.Write([]byte("a\nb\n"))
	require.NoError(t, err)
	assert.Equal(t, len("a\nb\n"), n, "the queue path never reports a short write")

	_, err = w.Write([]byte("c\n"))
	require.NoError(t, err)

	assert.Empty(t, buf.String(), "active writes queue instead of reaching the fallback")

	lines, _ := w.Peek()
	assert.Equal(t, []string{"a", "b", "c"}, lines, "lines peek oldest first")

	again, _ := w.Peek()
	assert.Equal(t, lines, again, "a peek leaves the queue intact")
}

func TestLogWriter_CommitRemovesOnlyThePeekedLines(t *testing.T) {
	t.Parallel()

	w := progress.NewLogWriter(&bytes.Buffer{})
	w.Activate()

	_, err := w.Write([]byte("one\n"))
	require.NoError(t, err)

	peeked, cursor := w.Peek()
	require.Equal(t, []string{"one"}, peeked)

	// A line arriving between the peek and its commit must survive the commit.
	_, err = w.Write([]byte("two\n"))
	require.NoError(t, err)

	w.Commit(cursor)

	lines, _ := w.Peek()
	assert.Equal(t, []string{"two"}, lines, "the commit removed only what its peek saw")
}

func TestLogWriter_DeactivateFlushesUncommitted(t *testing.T) {
	t.Parallel()

	// Lines queued (even peeked) but never committed must reach the fallback
	// rather than vanish, and later writes go straight through.
	buf := &bytes.Buffer{}
	w := progress.NewLogWriter(buf)

	w.Activate()

	_, err := w.Write([]byte("queued during shutdown\n"))
	require.NoError(t, err)

	_, _ = w.Peek()

	w.Deactivate()
	assert.Equal(t, "queued during shutdown\n", buf.String())

	_, err = w.Write([]byte("after\n"))
	require.NoError(t, err)
	assert.Equal(t, "queued during shutdown\nafter\n", buf.String())
}

func TestLogWriter_DeactivateSkipsCommittedLines(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	w := progress.NewLogWriter(buf)

	w.Activate()

	_, err := w.Write([]byte("printed\n"))
	require.NoError(t, err)

	_, cursor := w.Peek()
	w.Commit(cursor)

	w.Deactivate()
	assert.Empty(t, buf.String(), "committed lines are not repeated by the fallback flush")
}

func TestLogWriter_OverflowDropsOldestAndMarks(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	w := progress.NewLogWriter(buf)

	w.Activate()

	over := progress.MaxQueuedLogLines + 3
	for i := range over {
		_, err := fmt.Fprintf(w, "line-%05d\n", i)
		require.NoError(t, err)
	}

	lines, cursor := w.Peek()
	require.Len(t, lines, progress.MaxQueuedLogLines+1, "capped queue plus the overflow marker")
	assert.Equal(t, "… 3 log lines dropped", lines[0], "the marker counts the lines that gave way")
	assert.Equal(t, "line-00003", lines[1], "the oldest lines gave way")
	assert.Equal(t, fmt.Sprintf("line-%05d", over-1), lines[len(lines)-1], "the newest line survives")

	w.Commit(cursor)

	lines, _ = w.Peek()
	assert.Empty(t, lines, "the commit clears the batch and its marker")
}

func TestSplitLogLines(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		want []string
		line string
	}{
		"single line trailing newline": {line: "hello\n", want: []string{"hello"}},
		"multiple lines":               {line: "a\nb\nc\n", want: []string{"a", "b", "c"}},
		"no trailing newline":          {line: "no newline", want: []string{"no newline"}},
		"embedded blank line":          {line: "a\n\nb\n", want: []string{"a", "", "b"}},
		"only newline":                 {line: "\n", want: nil},
		"empty":                        {line: "", want: nil},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, progress.SplitLogLines([]byte(tc.line)))
		})
	}
}
