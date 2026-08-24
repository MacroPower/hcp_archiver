package remote_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote/remotetest"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func TestDownloadVerified(t *testing.T) {
	t.Parallel()

	body := []byte("restored roll-up content\n")
	other := []byte("superseded roll-up content")

	tests := map[string]struct {
		info    remote.ObjectInfo
		errIs   error
		errText string
	}{
		"sha256 match": {
			info: remote.ObjectInfo{Size: int64(len(body)), SHA256: sha256Hex(body)},
		},
		"sha256 mismatch": {
			info:  remote.ObjectInfo{Size: int64(len(body)), SHA256: sha256Hex(other)},
			errIs: remote.ErrDigestMismatch,
		},
		"md5 match when no sha256 is recorded": {
			info: remote.ObjectInfo{Size: int64(len(body)), MD5: remotetest.MD5Sum(body)},
		},
		"md5 mismatch": {
			info:  remote.ObjectInfo{Size: int64(len(body)), MD5: remotetest.MD5Sum(other)},
			errIs: remote.ErrDigestMismatch,
		},
		"no digest verifies on length alone": {
			info: remote.ObjectInfo{Size: int64(len(body))},
		},
		"short object refuses on length": {
			info:    remote.ObjectInfo{Size: int64(len(body)) + 10, SHA256: sha256Hex(body)},
			errText: "served",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, fake := newClient(t, remote.Config{})
			fake.SetObject("acme/rollup.ndjson", remotetest.Object{Data: body})

			var buf bytes.Buffer

			n, err := client.DownloadVerified(t.Context(), "acme/rollup.ndjson", tt.info, &buf)

			switch {
			case tt.errIs != nil:
				require.ErrorIs(t, err, tt.errIs)
			case tt.errText != "":
				require.ErrorContains(t, err, tt.errText)
			default:
				require.NoError(t, err)
				assert.Equal(t, int64(len(body)), n)
				assert.Equal(t, body, buf.Bytes())
			}
		})
	}
}
