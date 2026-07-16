package progress_test

import (
	"bytes"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/progress"
)

func TestThroughputDerivesFromWireBytes(t *testing.T) {
	t.Parallel()

	// A large object mid-transfer moves the wire counter while the committed
	// tally holds still; the panel's windowed rate must read the live wire flow
	// rather than zero.
	flat := manifest.Tally{BytesDownloaded: 100}

	snaps := []progress.PanelSnapshot{
		{Elapsed: 1 * time.Second, WireBytes: 0, Tally: flat},
		{Elapsed: 2 * time.Second, WireBytes: 1024, Tally: flat},
		{Elapsed: 3 * time.Second, WireBytes: 2048, Tally: flat},
	}

	rate := progress.ObserveThroughput(snaps)
	assert.InDelta(t, 1024.0, rate, 0.001, "the rate derives from wire-byte deltas over the window")
}

func TestThroughputStallsToZeroOnFlatWireBytes(t *testing.T) {
	t.Parallel()

	// The inverse honesty check: a truly silent wire reads zero even while the
	// committed tally would have suggested a stale lifetime average.
	snaps := []progress.PanelSnapshot{
		{Elapsed: 1 * time.Second, WireBytes: 4096, Tally: manifest.Tally{BytesDownloaded: 100}, Rate: 99},
		{Elapsed: 2 * time.Second, WireBytes: 4096, Tally: manifest.Tally{BytesDownloaded: 200}, Rate: 99},
	}

	rate := progress.ObserveThroughput(snaps)
	assert.Zero(t, rate, "a silent wire reads zero regardless of committed bytes")
}

func TestReporterWireBytesSources(t *testing.T) {
	t.Parallel()

	source := fakeSource{tally: manifest.Tally{BytesDownloaded: 42}}

	t.Run("without a counter falls back to committed bytes", func(t *testing.T) {
		t.Parallel()

		r := progress.New(&bytes.Buffer{}, config.ProgressModeQuiet, source)

		assert.Equal(t, int64(42), progress.TakeWireBytes(r))
		require.NoError(t, r.Report(), "a reporter without a wire counter still reports")
	})

	t.Run("with a counter reads the counter", func(t *testing.T) {
		t.Parallel()

		counter := new(atomic.Int64)
		counter.Store(7)

		r := progress.New(&bytes.Buffer{}, config.ProgressModeQuiet, source,
			progress.WithWireBytes(counter))

		assert.Equal(t, int64(7), progress.TakeWireBytes(r))
	})
}

func TestUploadThroughputDerivesFromUploadWireBytes(t *testing.T) {
	t.Parallel()

	// A large object mid-upload moves the upload wire counter while the
	// committed remote tally holds still; the remote line's windowed rate must
	// read the live wire flow rather than zero.
	flat := progress.RemoteStats{UploadedBytes: 100}

	snaps := []progress.PanelSnapshot{
		{Elapsed: 1 * time.Second, UploadWireBytes: 0, Remote: flat, HasRemote: true},
		{Elapsed: 2 * time.Second, UploadWireBytes: 1024, Remote: flat, HasRemote: true},
		{Elapsed: 3 * time.Second, UploadWireBytes: 2048, Remote: flat, HasRemote: true},
	}

	rate := progress.ObserveUploadThroughput(snaps)
	assert.InDelta(t, 1024.0, rate, 0.001, "the rate derives from upload wire-byte deltas over the window")
}

func TestUploadThroughputStallsToZeroOnFlatUploadWireBytes(t *testing.T) {
	t.Parallel()

	// The inverse honesty check: an idle mirror reads zero even while the
	// committed remote tally would have suggested a stale lifetime average.
	snaps := []progress.PanelSnapshot{
		{
			Elapsed: 1 * time.Second, UploadWireBytes: 4096,
			Remote: progress.RemoteStats{UploadedBytes: 100}, HasRemote: true, UploadRate: 99,
		},
		{
			Elapsed: 2 * time.Second, UploadWireBytes: 4096,
			Remote: progress.RemoteStats{UploadedBytes: 200}, HasRemote: true, UploadRate: 99,
		},
	}

	rate := progress.ObserveUploadThroughput(snaps)
	assert.Zero(t, rate, "an idle mirror reads zero regardless of committed remote bytes")
}

func TestReporterUploadWireBytesSources(t *testing.T) {
	t.Parallel()

	source := fakeSource{}
	stats := func() progress.RemoteStats {
		return progress.RemoteStats{UploadedBytes: 42}
	}

	t.Run("without a counter falls back to committed remote bytes", func(t *testing.T) {
		t.Parallel()

		r := progress.New(&bytes.Buffer{}, config.ProgressModeQuiet, source,
			progress.WithRemoteStats(stats))

		assert.Equal(t, int64(42), progress.TakeUploadWireBytes(r))
		require.NoError(t, r.Report(), "a reporter without an upload wire counter still reports")
	})

	t.Run("with a counter reads the counter", func(t *testing.T) {
		t.Parallel()

		counter := new(atomic.Int64)
		counter.Store(7)

		r := progress.New(&bytes.Buffer{}, config.ProgressModeQuiet, source,
			progress.WithRemoteStats(stats), progress.WithUploadWireBytes(counter))

		assert.Equal(t, int64(7), progress.TakeUploadWireBytes(r))
	})
}
