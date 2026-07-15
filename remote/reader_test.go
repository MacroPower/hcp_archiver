package remote_test

import (
	"archive/zip"
	"bytes"
	"context"
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
