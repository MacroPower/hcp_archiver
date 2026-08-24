package remote_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote/remotetest"
)

// tarball is a download fixture long enough to span several copy buffers, so a
// mid-body failure lands inside it rather than between requests.
var tarball = []byte(strings.Repeat("configuration tarball bytes\n", 4000))

func TestDownloadWholeObject(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("cv-1.tar.gz", remotetest.Object{Data: tarball})

	var got bytes.Buffer

	n, err := client.Download(t.Context(), "cv-1.tar.gz", int64(len(tarball)), &got)
	require.NoError(t, err)
	assert.EqualValues(t, len(tarball), n)
	assert.Equal(t, tarball, got.Bytes())

	require.Len(t, fake.Ranges(), 1, "an untroubled download is one request")
	assert.Equal(t, remotetest.Range{Offset: 0, Length: int64(len(tarball))}, fake.Ranges()[0],
		"the request is a bounded range, not an open-ended read")
}

func TestDownloadAbsent(t *testing.T) {
	t.Parallel()

	client, _ := newClient(t, remote.Config{})

	var got bytes.Buffer

	_, err := client.Download(t.Context(), "missing.tar.gz", 12, &got)
	require.ErrorIs(t, err, remote.ErrNotFound)
}

func TestDownloadEmptyObject(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("empty.tar.gz", remotetest.Object{Data: nil})

	var got bytes.Buffer

	// A stub recording a zero size is legal, and a zero-length range is a
	// request the stores answer with an error rather than an empty body.
	n, err := client.Download(t.Context(), "empty.tar.gz", 0, &got)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, got.Bytes())
	assert.Empty(t, fake.Ranges(), "an empty object costs no request")
}

func TestDownloadBodyFailsAfterLastByte(t *testing.T) {
	t.Parallel()

	client, fake := newRetryClient(t, 3)
	fake.SetObject("cv-1.tar.gz", remotetest.Object{Data: tarball})

	// The store delivered everything and then reported a fault, the shape
	// io.Copy hands back as bytes plus an error. Resuming would ask for a range
	// starting at the object's end, which the stores refuse outright, turning a
	// download that in fact succeeded into a permanent failure.
	fake.RangeBodyErr = errors.New("connection reset")
	fake.RangeBodyErrAfter = int64(len(tarball))
	fake.RangeBodyErrN = 1

	var got bytes.Buffer

	n, err := client.Download(t.Context(), "cv-1.tar.gz", int64(len(tarball)), &got)
	require.NoError(t, err)
	assert.EqualValues(t, len(tarball), n)
	assert.Equal(t, tarball, got.Bytes())
	assert.Len(t, fake.Ranges(), 1, "nothing is left to re-request")
}

func TestDownloadStaleSizeBodyFailsAfterTrueEnd(t *testing.T) {
	t.Parallel()

	client, fake := newRetryClient(t, 3)
	fake.SetObject("cv-1.tar.gz", remotetest.Object{Data: tarball})

	// The caller holds a stale size for an object a foreign overwrite has since
	// shortened, and the body dies after delivering every byte the object
	// actually holds. Resuming against the caller's size would ask for a range
	// past the true end, a 416 that would burn the whole retry budget; the
	// pinned version's own length says the transfer is complete, and the
	// short-serve contract hands the caller the count to judge.
	fake.RangeBodyErr = errors.New("connection reset")
	fake.RangeBodyErrAfter = int64(len(tarball))
	fake.RangeBodyErrN = 1

	var got bytes.Buffer

	n, err := client.Download(t.Context(), "cv-1.tar.gz", int64(len(tarball))+200, &got)
	require.NoError(t, err)
	assert.EqualValues(t, len(tarball), n)
	assert.Equal(t, tarball, got.Bytes())
	assert.Len(t, fake.Ranges(), 1, "nothing is left to re-request")
}

func TestDownloadResumesMidBodyFailure(t *testing.T) {
	t.Parallel()

	const delivered = 5000

	client, fake := newRetryClient(t, 3)
	fake.SetObject("cv-1.tar.gz", remotetest.Object{Data: tarball})

	fake.RangeBodyErr = errors.New("connection reset")
	fake.RangeBodyErrAfter = delivered
	fake.RangeBodyErrN = 1

	var got bytes.Buffer

	n, err := client.Download(t.Context(), "cv-1.tar.gz", int64(len(tarball)), &got)
	require.NoError(t, err)
	assert.EqualValues(t, len(tarball), n)

	// The assembled output is the object, not the object with a duplicated
	// prefix: the destination has no rewind, so the retry must resume rather
	// than restart.
	assert.Equal(t, tarball, got.Bytes())

	ranges := fake.Ranges()
	require.Len(t, ranges, 2)
	assert.Equal(t, remotetest.Range{Offset: 0, Length: int64(len(tarball))}, ranges[0])

	// The second request asks for exactly the remainder, from where the first
	// body died. The fake serves whole reads, so the delivered count is the
	// buffer boundary at or past the failure point rather than the byte itself.
	assert.Positive(t, ranges[1].Offset, "the resume starts past what already landed")
	assert.GreaterOrEqual(t, ranges[1].Offset, int64(delivered))
	assert.Equal(t, int64(len(tarball)), ranges[1].Offset+ranges[1].Length,
		"the resumed range ends at the object's end")
}

// A download's first attempt fails mid-body and the resume finds another
// object under the key, the race a sweep's re-upload runs against a viewer's
// fetch. The resume may not append that object's tail to the prefix already
// delivered: the splice would hash to neither version, and every caller reads
// such a stream as a healthy object gone corrupt.
//
// The after object stands in for the re-upload, written over the key as the
// resume opens its range; delivered is how many bytes the first attempt's
// body serves before it fails, a zero failing it before its first byte
// reaches the destination.
func TestDownloadObjectReplacedMidTransfer(t *testing.T) {
	t.Parallel()

	var (
		stored  = time.Date(2026, time.August, 14, 9, 0, 0, 0, time.UTC)
		rewrote = stored.Add(time.Minute)
		// The replacement matches tarball's length exactly, so nothing but the
		// modification time distinguishes it: a splice of the two would measure
		// right and read wrong.
		replacement = []byte(strings.Repeat("a different tarball's bytes\n", 4000))
	)

	tests := map[string]struct {
		after     *remotetest.Object
		want      []byte
		err       error
		delivered int64
	}{
		"replaced with content of the same length": {
			delivered: 5000,
			after:     &remotetest.Object{Data: replacement, ModTime: rewrote},
			err:       remote.ErrObjectChanged,
		},
		"replaced with a shorter object": {
			delivered: 5000,
			after:     &remotetest.Object{Data: replacement[:9000], ModTime: stored},
			err:       remote.ErrObjectChanged,
		},
		"untouched between the attempts": {
			delivered: 5000,
			want:      tarball,
		},
		"replaced before a byte lands": {
			// Nothing reached the destination, so there is no prefix to splice
			// onto and the resume is free to serve whatever the key now holds.
			delivered: 0,
			after:     &remotetest.Object{Data: replacement, ModTime: rewrote},
			want:      replacement,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, fake := newRetryClient(t, 3)
			fake.SetObject("cv-1.tar.gz", remotetest.Object{Data: tarball, ModTime: stored})

			fake.RangeBodyErr = errors.New("connection reset")
			fake.RangeBodyErrAfter = test.delivered
			fake.RangeBodyErrN = 1

			opens := 0
			fake.RangeHook = func(context.Context) {
				opens++

				if opens == 2 && test.after != nil {
					fake.SetObject("cv-1.tar.gz", *test.after)
				}
			}

			var got bytes.Buffer

			n, err := client.Download(t.Context(), "cv-1.tar.gz", int64(len(tarball)), &got)

			require.Len(t, fake.Ranges(), 2,
				"the first attempt fails mid-body and the resume settles the transfer either way")

			if test.err != nil {
				require.ErrorIs(t, err, test.err)
				assert.True(t, bytes.HasPrefix(tarball, got.Bytes()),
					"only the version the download began on reached the destination")
				assert.EqualValues(t, got.Len(), n, "the count is what really landed")

				return
			}

			require.NoError(t, err)
			assert.EqualValues(t, len(test.want), n)
			assert.Equal(t, test.want, got.Bytes(), "the bytes are one version's, whole")
		})
	}
}

// errWriter is a destination that refuses every byte, the shape a full disk
// under an extract target takes.
type errWriter struct {
	err error
}

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }

func TestDownloadDoesNotRetryDestinationFailure(t *testing.T) {
	t.Parallel()

	client, fake := newRetryClient(t, 3)
	fake.SetObject("cv-1.tar.gz", remotetest.Object{Data: tarball})

	full := errors.New("no space left on device")

	_, err := client.Download(t.Context(), "cv-1.tar.gz", int64(len(tarball)), errWriter{err: full})
	require.ErrorIs(t, err, full)

	assert.Len(t, fake.Ranges(), 1,
		"the store did nothing wrong; re-reading the object would fail on the same write")
}

func TestDownloadShortObject(t *testing.T) {
	t.Parallel()

	client, fake := newRetryClient(t, 3)
	fake.SetObject("cv-1.tar.gz", remotetest.Object{Data: []byte("short")})

	var got bytes.Buffer

	// A store that serves less than the recorded size and ends cleanly is not
	// having a transient problem; retrying would short-serve identically. The
	// count comes back for the caller to judge.
	n, err := client.Download(t.Context(), "cv-1.tar.gz", 64, &got)
	require.NoError(t, err)
	assert.EqualValues(t, 5, n)
	assert.Equal(t, "short", got.String())
	assert.Len(t, fake.Ranges(), 1)
}
