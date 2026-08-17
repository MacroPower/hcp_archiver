package remote_test

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote/remotetest"
)

// newCountingClient builds a [*remote.Client] over a fresh fake store with an
// injected wire-byte counter and the given retry budget (no backoff delay).
func newCountingClient(t *testing.T, retries int) (*remote.Client, *remotetest.Fake, *atomic.Int64) {
	t.Helper()

	fake := remotetest.New()
	counter := new(atomic.Int64)

	client, err := remote.New(t.Context(), remote.Config{},
		remote.WithBucket(fake.Bucket()), remote.WithRetry(retries, 0),
		remote.WithWireBytes(counter))
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	return client, fake, counter
}

func TestUploadCountsWireBytes(t *testing.T) {
	t.Parallel()

	client, _, counter := newCountingClient(t, 0)

	body := []byte("sealed bundle bytes")
	err := client.Upload(t.Context(), "k", bytes.NewReader(body), int64(len(body)), remote.Digests{})
	require.NoError(t, err)

	assert.Equal(t, int64(len(body)), counter.Load(),
		"an upload counts every byte it streams")
}

func TestUploadRetryRecountsWireBytes(t *testing.T) {
	t.Parallel()

	// A retried attempt rewinds and re-streams the whole body, so the wire
	// counter tallies both passes: it measures bytes moved, not bytes
	// committed.
	client, fake, counter := newCountingClient(t, 2)
	fake.PutErr = errors.New("injected transient commit failure")
	fake.PutErrN = 1

	body := []byte("sealed bundle bytes")
	err := client.Upload(t.Context(), "k", bytes.NewReader(body), int64(len(body)), remote.Digests{})
	require.NoError(t, err)

	assert.Equal(t, int64(2*len(body)), counter.Load(),
		"a retried upload recounts the re-streamed body")
}

func TestPutCountsWireBytes(t *testing.T) {
	t.Parallel()

	client, _, counter := newCountingClient(t, 0)

	data := []byte("search layer file")
	require.NoError(t, client.Put(t.Context(), "k", data))

	assert.Equal(t, int64(len(data)), counter.Load(),
		"a put counts its whole body once on success")
}
