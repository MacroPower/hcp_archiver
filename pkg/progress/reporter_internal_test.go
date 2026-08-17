package progress

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tea "charm.land/bubbletea/v2"
)

func TestWithLogSink_DropsNilSinks(t *testing.T) {
	t.Parallel()

	sink := NewLogWriter(io.Discard)

	tests := map[string]struct {
		sink LogSink
		want LogSink
	}{
		"an untyped nil leaves the reporter without a sink": {sink: nil},
		"a nil writer does not survive as a live interface": {sink: (*LogWriter)(nil)},
		"a real sink is kept":                               {sink: sink, want: sink},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// A typed nil passes every later `sink != nil` check, so the panel
			// would reach it and dereference a nil receiver on Activate.
			var r Reporter

			WithLogSink(tc.sink)(&r)

			assert.Equal(t, tc.want, r.sink)
		})
	}
}

// fakeTerminal is a writer carrying the shape Bubble Tea inspects an output
// for (see [terminalFile]), so the guard's passthrough can be exercised
// without a real tty.
type fakeTerminal struct {
	buf strings.Builder
	fd  uintptr
}

//nolint:wrapcheck // A stand-in terminal reports what its buffer reports.
func (f *fakeTerminal) Write(p []byte) (int, error) { return f.buf.Write(p) }
func (f *fakeTerminal) Read([]byte) (int, error)    { return 0, io.EOF }
func (f *fakeTerminal) Close() error                { return nil }
func (f *fakeTerminal) Fd() uintptr                 { return f.fd }

func TestTermGuard_RevocationSilencesAnAbandonedProgram(t *testing.T) {
	t.Parallel()

	// A program the shutdown escalation abandoned may unwedge later and finish
	// unwinding onto a terminal its successor now owns; the revocation is what
	// keeps those writes off the shared screen.
	buf := &strings.Builder{}
	out, guard := guardTerminal(buf)

	_, err := io.WriteString(out, "live frame")
	require.NoError(t, err)

	guard.revoked.Store(true)

	n, err := io.WriteString(out, "zombie teardown")
	require.NoError(t, err, "a revoked program unwinds rather than erroring")
	assert.Equal(t, len("zombie teardown"), n, "a discarded write is not a short write")

	assert.Equal(t, "live frame", buf.String(),
		"the abandoned program's writes never reach the terminal")
}

func TestGuardTerminal_CarriesTheTerminalShape(t *testing.T) {
	t.Parallel()

	// Bubble Tea queries the window size and detects the color profile through
	// the output's descriptor, so a guarded terminal must still read as one:
	// only its writes are intercepted.
	f := &fakeTerminal{fd: 7}
	out, guard := guardTerminal(f)

	file, ok := out.(terminalFile)
	require.True(t, ok, "the guarded writer is still a terminal file")
	assert.Equal(t, uintptr(7), file.Fd(), "the real descriptor carries through")

	guard.revoked.Store(true)

	_, err := io.WriteString(out, "zombie teardown")
	require.NoError(t, err)
	assert.Empty(t, f.buf.String(), "revocation guards the terminal's writes too")
}

func TestTuiError(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in      error
		wantNil bool
	}{
		"nil passes through": {
			in:      nil,
			wantNil: true,
		},
		"the escalation's bare kill reads clean": {
			in:      tea.ErrProgramKilled,
			wantNil: true,
		},
		"a kill carrying a context cancel reads clean": {
			in:      fmt.Errorf("%w: %w", tea.ErrProgramKilled, context.Canceled),
			wantNil: true,
		},
		"a recovered panic surfaces despite riding the kill sentinel": {
			in:      fmt.Errorf("%w: %w", tea.ErrProgramKilled, tea.ErrProgramPanic),
			wantNil: false,
		},
		"any other program error wraps": {
			in:      assert.AnError,
			wantNil: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := tuiError(tc.in)

			if tc.wantNil {
				assert.NoError(t, got)

				return
			}

			require.Error(t, got)
			assert.ErrorIs(t, got, tc.in, "the cause stays reachable through the wrap")
		})
	}
}

func TestSnapshotEta(t *testing.T) {
	t.Parallel()

	t.Run("extrapolates linearly from the completed units", func(t *testing.T) {
		t.Parallel()

		s := snapshot{total: 10, completed: 2, phaseElapsed: 20 * time.Second}

		got, ok := s.eta()
		require.True(t, ok)
		assert.Equal(t, 80*time.Second, got, "10s per unit over the 8 remaining")
	})

	t.Run("saturates instead of overflowing on an extreme count", func(t *testing.T) {
		t.Parallel()

		// The honest product overflows the int64 nanosecond duration; without a
		// guard it wraps negative and renders as a bogus near-zero eta.
		s := snapshot{total: math.MaxInt, completed: 1, phaseElapsed: 100 * time.Second}

		got, ok := s.eta()
		require.True(t, ok)
		assert.GreaterOrEqual(t, got, 100*time.Hour,
			"an overflowing extrapolation saturates rather than wrapping negative")
	})
}
