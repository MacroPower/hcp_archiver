package remote

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteWindowScalesWithBodySize(t *testing.T) {
	t.Parallel()

	c := &Client{stallTimeout: 2 * time.Minute}

	tests := map[string]struct {
		size int64
		want time.Duration
	}{
		// A small body adds nothing measurable: the base window rules.
		"tiny body keeps the base window": {size: 32, want: 2 * time.Minute},
		// A 16 MiB body (S3's single-shot threshold, whose whole transfer
		// happens inside the writer's commit with no observable progress)
		// widens the window enough to cross a 32 KiB/s link.
		"single-shot body widens by size": {size: 16 << 20, want: 2*time.Minute + 512*time.Second},
		// The sync path's largest in-memory body.
		"stream-threshold body": {size: 32 << 20, want: 2*time.Minute + 1024*time.Second},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, c.writeWindow(tc.size))
		})
	}
}

func TestWriteWindowDisabledWatchdog(t *testing.T) {
	t.Parallel()

	c := &Client{stallTimeout: 0}

	assert.Zero(t, c.writeWindow(1<<30), "a disabled watchdog stays disabled at any size")
}

func TestRunAttemptWrapsOnlyTheWatchdogCancellation(t *testing.T) {
	t.Parallel()

	c := &Client{}

	t.Run("a stalled attempt wraps errStalled", func(t *testing.T) {
		t.Parallel()

		err := c.runAttempt(t.Context(), 10*time.Millisecond, func(ctx context.Context, _ func()) error {
			<-ctx.Done()

			return ctx.Err()
		})
		require.ErrorIs(t, err, errStalled)
	})

	t.Run("a store verdict landing at the watchdog keeps its shape", func(t *testing.T) {
		t.Parallel()

		verdict := errors.New("permission denied")

		// The op ignores the watchdog's cancellation and returns the store's
		// own verdict after the window elapsed, the in-flight-response race.
		// Wrapped in errStalled it would classify transient and be retried for
		// the whole budget; it must surface as itself.
		err := c.runAttempt(t.Context(), 10*time.Millisecond, func(ctx context.Context, _ func()) error {
			<-ctx.Done()
			time.Sleep(5 * time.Millisecond)

			return verdict
		})
		require.ErrorIs(t, err, verdict)
		assert.NotErrorIs(t, err, errStalled)
	})
}
