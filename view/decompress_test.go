package view_test

import (
	"bytes"
	"compress/flate"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/view"
)

// deflate compresses payload the way a sealed bundle member is stored, so a
// test can feed the compressed span back through the decompressor.
func deflate(t *testing.T, payload []byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	w, err := flate.NewWriter(&buf, flate.BestCompression)
	require.NoError(t, err)

	_, err = w.Write(payload)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	return buf.Bytes()
}

func TestDecompressMemberBounded(t *testing.T) {
	t.Parallel()

	const limit = 1 << 10

	t.Run("a member under the cap inflates whole", func(t *testing.T) {
		t.Parallel()

		payload := bytes.Repeat([]byte("a"), limit)

		data, err := view.DecompressMemberBoundedForTest(view.DeflateMethod, deflate(t, payload), limit)
		require.NoError(t, err)
		assert.Equal(t, payload, data)
	})

	t.Run("a member past the cap is rejected, not materialized", func(t *testing.T) {
		t.Parallel()

		// A tiny compressed span that inflates far past the cap -- the decompression
		// bomb the bound exists to stop. A run of one byte compresses enormously, so
		// the span itself stays well under the bundle-size guard.
		bomb := deflate(t, bytes.Repeat([]byte("a"), 64<<10))
		require.Less(t, len(bomb), limit, "the bomb's compressed span is smaller than its expansion cap")

		_, err := view.DecompressMemberBoundedForTest(view.DeflateMethod, bomb, limit)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds the", "an oversized inflate is rejected by the cap")
	})
}
