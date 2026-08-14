package remote_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/remote/remotetest"
)

// readerAt adapts [remote.Client.ReadAt] to [io.ReaderAt] under a fixed
// context, the shape zip parsing needs, as the view layer's adapter does.
type readerAt struct {
	ctx    context.Context
	client *remote.Client
	key    string
	size   int64
}

func (r readerAt) ReadAt(p []byte, off int64) (int, error) {
	//nolint:wrapcheck // A transparent pass-through; the client already wraps.
	return r.client.ReadAt(r.ctx, r.key, r.size, p, off)
}

// buildZip renders members (name -> content) into an in-memory zip, half the
// members deflated and half stored, mirroring the logs and state bundles.
func buildZip(t *testing.T, members map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)
	compress := true

	for name, content := range members {
		method := zip.Store
		if compress {
			method = zip.Deflate
		}

		compress = !compress

		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		require.NoError(t, err)

		_, err = w.Write([]byte(content))
		require.NoError(t, err)
	}

	require.NoError(t, zw.Close())

	return buf.Bytes()
}

func TestReadAtServesZip(t *testing.T) {
	t.Parallel()

	members := map[string]string{
		"runs/run-1/plan.log":  strings.Repeat("plan output line\n", 200),
		"runs/run-2/apply.log": "short apply",
	}

	bundle := buildZip(t, members)

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("logs.gen0001.zip", remotetest.Object{Data: bundle})

	size := int64(len(bundle))
	ra := readerAt{ctx: t.Context(), client: client, key: "logs.gen0001.zip", size: size}

	zr, err := zip.NewReader(ra, size)
	require.NoError(t, err, "the central directory should parse over ranged reads")

	for name, want := range members {
		rc, openErr := zr.Open(name)
		require.NoError(t, openErr)

		got, readErr := io.ReadAll(rc)
		require.NoError(t, readErr)
		require.NoError(t, rc.Close())
		assert.Equal(t, want, string(got), "member %s should read back intact", name)
	}

	for _, r := range fake.Ranges() {
		assert.GreaterOrEqual(t, r.Length, int64(0),
			"every read should be a bounded ranged request, never a full download")
	}
}

func TestReadAtAbsent(t *testing.T) {
	t.Parallel()

	client, _ := newClient(t, remote.Config{})

	buf := make([]byte, 4)
	_, err := client.ReadAt(t.Context(), "missing.zip", 12, buf, 0)
	require.ErrorIs(t, err, remote.ErrNotFound)
}

func TestReadAtShortObject(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		err     error
		want    string
		stored  int
		size    int64
		off     int64
		buf     int
		reopens int
	}{
		"object truncated below the read": {
			stored: 10, size: 100, off: 0, buf: 100,
			err: remote.ErrShortObject,
		},
		"span runs past the object end": {
			stored: 10, size: 20, off: 8, buf: 8,
			err: remote.ErrShortObject,
		},
		"span inside a truncated object": {
			stored: 10, size: 100, off: 2, buf: 5,
			want: "23456",
		},
		"object longer than the size read against": {
			stored: 20, size: 10, off: 0, buf: 10,
			want: "0123456789",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// The retry budget is armed, so an attempt count above one is the
			// classifier reading a permanent length as a transient blip.
			client, fake := newRetryClient(t, remote.DefaultRetries)
			data := []byte(strings.Repeat("0123456789", 1+tc.stored/10))
			fake.SetObject("k", remotetest.Object{Data: data[:tc.stored]})

			buf := make([]byte, tc.buf)

			n, err := client.ReadAt(t.Context(), "k", tc.size, buf, tc.off)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				assert.Zero(t, n, "a span past the object's end fills nothing")
				assert.Len(t, fake.Ranges(), 1,
					"a length that cannot change should settle on the first attempt")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, string(buf[:n]))
		})
	}
}

func TestReadAtRetriesShortBody(t *testing.T) {
	t.Parallel()

	client, fake := newRetryClient(t, remote.DefaultRetries)
	fake.SetObject("k", remotetest.Object{Data: []byte("0123456789")})

	fake.RangeBodyErr = errors.New("connection reset")
	fake.RangeBodyErrN = 1
	fake.RangeBodyErrAfter = 3

	buf := make([]byte, 10)

	n, err := client.ReadAt(t.Context(), "k", 10, buf, 0)
	require.NoError(t, err, "a body cut short of an object long enough is transient")
	assert.Equal(t, 10, n)
	assert.Equal(t, "0123456789", string(buf))
	assert.Len(t, fake.Ranges(), 2, "the reopened range should refill p from off")
}

func TestReadAtEdges(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("k", remotetest.Object{Data: []byte("0123456789")})

	t.Run("clipped at end", func(t *testing.T) {
		t.Parallel()

		buf := make([]byte, 6)
		n, err := client.ReadAt(t.Context(), "k", 10, buf, 7)
		require.ErrorIs(t, err, io.EOF, "a read clipped by the object's end reports EOF")
		assert.Equal(t, 3, n)
		assert.Equal(t, "789", string(buf[:n]))
	})

	t.Run("past end", func(t *testing.T) {
		t.Parallel()

		n, err := client.ReadAt(t.Context(), "k", 10, make([]byte, 1), 10)
		require.ErrorIs(t, err, io.EOF)
		assert.Zero(t, n)
	})

	t.Run("full read", func(t *testing.T) {
		t.Parallel()

		buf := make([]byte, 10)
		n, err := client.ReadAt(t.Context(), "k", 10, buf, 0)
		require.NoError(t, err)
		assert.Equal(t, 10, n)
		assert.Equal(t, "0123456789", string(buf))
	})
}
