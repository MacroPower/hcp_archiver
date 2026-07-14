package remote_test

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/remote/remotetest"
)

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

func TestReaderAtServesZip(t *testing.T) {
	t.Parallel()

	members := map[string]string{
		"runs/run-1/plan.log":  strings.Repeat("plan output line\n", 200),
		"runs/run-2/apply.log": "short apply",
	}

	bundle := buildZip(t, members)

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("logs.gen0001.zip", remotetest.Object{Data: bundle})

	size := int64(len(bundle))
	zr, err := zip.NewReader(client.ReaderAt(t.Context(), "logs.gen0001.zip", size), size)
	require.NoError(t, err, "the central directory should parse over ranged GETs")

	for name, want := range members {
		rc, openErr := zr.Open(name)
		require.NoError(t, openErr)

		got, readErr := io.ReadAll(rc)
		require.NoError(t, readErr)
		require.NoError(t, rc.Close())
		assert.Equal(t, want, string(got), "member %s should read back intact", name)
	}

	for _, r := range fake.GetRanges() {
		assert.NotEmpty(t, r, "every read should be a ranged GET, never a full download")
	}
}

func TestReaderAtRestoreRequired(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("cold.zip", remotetest.Object{
		Data:         []byte("frozen bytes"),
		StorageClass: "DEEP_ARCHIVE",
	})

	buf := make([]byte, 4)
	_, err := client.ReaderAt(t.Context(), "cold.zip", 12).ReadAt(buf, 0)
	require.ErrorIs(t, err, remote.ErrRestoreRequired)
}

func TestReaderAtEdges(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("k", remotetest.Object{Data: []byte("0123456789")})

	ra := client.ReaderAt(t.Context(), "k", 10)

	t.Run("clipped at end", func(t *testing.T) {
		t.Parallel()

		buf := make([]byte, 6)
		n, err := ra.ReadAt(buf, 7)
		require.ErrorIs(t, err, io.EOF, "a read clipped by the object's end reports EOF")
		assert.Equal(t, 3, n)
		assert.Equal(t, "789", string(buf[:n]))
	})

	t.Run("past end", func(t *testing.T) {
		t.Parallel()

		n, err := ra.ReadAt(make([]byte, 1), 10)
		require.ErrorIs(t, err, io.EOF)
		assert.Zero(t, n)
	})

	t.Run("full read", func(t *testing.T) {
		t.Parallel()

		buf := make([]byte, 10)
		n, err := ra.ReadAt(buf, 0)
		require.NoError(t, err)
		assert.Equal(t, 10, n)
		assert.Equal(t, "0123456789", string(buf))
	})
}
