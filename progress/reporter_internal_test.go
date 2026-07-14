package progress

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
