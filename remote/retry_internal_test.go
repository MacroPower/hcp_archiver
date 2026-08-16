package remote

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
