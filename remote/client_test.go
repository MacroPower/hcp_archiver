package remote_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/remote/remotetest"
)

const testBucket = "test-bucket"

// newClient builds a [*remote.Client] over a fresh fake store.
func newClient(t *testing.T, cfg remote.Config) (*remote.Client, *remotetest.Fake) {
	t.Helper()

	cfg.Bucket = testBucket
	fake := remotetest.New(testBucket)

	client, err := remote.New(t.Context(), cfg, remote.WithS3API(fake))
	require.NoError(t, err)

	return client, fake
}

func TestNewMissingBucket(t *testing.T) {
	t.Parallel()

	_, err := remote.New(t.Context(), remote.Config{}, remote.WithS3API(remotetest.New("b")))
	require.ErrorIs(t, err, remote.ErrMissingBucket)
}

func TestUploadSingle(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{StorageClass: "DEEP_ARCHIVE"})

	body := []byte("sealed bundle bytes")
	err := client.Upload(t.Context(), "acme/bundles/logs.gen0001.zip", bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)

	obj, ok := fake.Object("acme/bundles/logs.gen0001.zip")
	require.True(t, ok, "object should be stored")
	assert.Equal(t, body, obj.Data)
	assert.Equal(t, "DEEP_ARCHIVE", obj.StorageClass, "configured storage class should be applied")
	assert.Equal(t, 1, fake.PutCalls(), "a small body should upload in one PutObject")
	assert.Equal(t, []string{"SHA256"}, fake.PutChecksums(), "writes should carry a server-validated checksum")
}

func TestUploadMultipart(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{PartSize: manager.MinUploadPartSize})

	// Two full parts plus a remainder.
	body := make([]byte, 2*manager.MinUploadPartSize+1024)
	_, err := rand.Read(body)
	require.NoError(t, err)

	err = client.Upload(t.Context(), "big.zip", bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)

	obj, ok := fake.Object("big.zip")
	require.True(t, ok, "object should be stored")
	assert.Equal(t, body, obj.Data, "multipart parts should reassemble byte for byte")
	assert.Equal(t, 0, fake.PutCalls(), "a large body should go multipart, not PutObject")
	assert.Equal(t, 1, fake.Completed())
	assert.Equal(t, 0, fake.OpenUploads(), "no multipart upload should be left open")
}

func TestUploadDisableChecksums(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{DisableChecksums: true})

	err := client.Upload(t.Context(), "k", bytes.NewReader([]byte("x")), 1)
	require.NoError(t, err)

	assert.Equal(t, []string{""}, fake.PutChecksums(), "checksums off should omit the checksum algorithm")
}

func TestUploadAbortsOnFailure(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{PartSize: manager.MinUploadPartSize})
	fake.UploadPartErr = errors.New("injected part failure")

	body := make([]byte, 2*manager.MinUploadPartSize)

	err := client.Upload(t.Context(), "big.zip", bytes.NewReader(body), int64(len(body)))
	require.Error(t, err)

	_, ok := fake.Object("big.zip")
	assert.False(t, ok, "a dead upload should store no object")
	assert.Positive(t, fake.Aborted(), "the dead multipart upload should be aborted")
	assert.Equal(t, 0, fake.OpenUploads(), "no multipart upload should be left open to accrue storage")
}

func TestHead(t *testing.T) {
	t.Parallel()

	restored := `ongoing-request="false", expiry-date="Fri, 21 Dec 2026 00:00:00 GMT"`

	tests := map[string]struct {
		obj  *remotetest.Object
		key  string
		want remote.ObjectInfo
		err  error
	}{
		"present standard": {
			obj:  &remotetest.Object{Data: []byte("abcd")},
			key:  "k",
			want: remote.ObjectInfo{Size: 4},
		},
		"absent": {
			key: "missing",
			err: remote.ErrNotFound,
		},
		"glacier unrestored": {
			obj:  &remotetest.Object{Data: []byte("ab"), StorageClass: "GLACIER"},
			key:  "k",
			want: remote.ObjectInfo{Size: 2, StorageClass: "GLACIER", Archived: true},
		},
		"deep archive restore in progress": {
			obj: &remotetest.Object{
				Data:         []byte("ab"),
				StorageClass: "DEEP_ARCHIVE",
				Restore:      `ongoing-request="true"`,
			},
			key:  "k",
			want: remote.ObjectInfo{Size: 2, StorageClass: "DEEP_ARCHIVE", Archived: true},
		},
		"glacier restored": {
			obj: &remotetest.Object{
				Data:         []byte("ab"),
				StorageClass: "GLACIER",
				Restore:      restored,
			},
			key:  "k",
			want: remote.ObjectInfo{Size: 2, StorageClass: "GLACIER", Archived: true, Restored: true},
		},
		"glacier instant retrieval reads directly": {
			obj:  &remotetest.Object{Data: []byte("ab"), StorageClass: "GLACIER_IR"},
			key:  "k",
			want: remote.ObjectInfo{Size: 2, StorageClass: "GLACIER_IR"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, fake := newClient(t, remote.Config{})
			if tt.obj != nil {
				fake.SetObject(tt.key, *tt.obj)
			}

			got, err := client.Head(t.Context(), tt.key)
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExists(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("present", remotetest.Object{Data: []byte("abc")})

	ok, info, err := client.Exists(t.Context(), "present")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, int64(3), info.Size)

	ok, _, err = client.Exists(t.Context(), "absent")
	require.NoError(t, err, "an absent object is a report, not an error")
	assert.False(t, ok)
}
