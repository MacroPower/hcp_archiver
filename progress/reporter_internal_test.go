package progress

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tea "charm.land/bubbletea/v2"
)

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
