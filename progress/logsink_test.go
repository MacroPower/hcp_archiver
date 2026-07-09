package progress_test

import (
	"bytes"
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

func TestLogWriter_FallbackAfterClear(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	w := progress.NewLogWriter(buf)

	// Clearing the program (nil) keeps the fallback path, the state a run leaves
	// behind once its TUI stops.
	w.SetProgram(nil)

	_, err := w.Write([]byte("after\n"))
	require.NoError(t, err)
	assert.Equal(t, "after\n", buf.String())
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
