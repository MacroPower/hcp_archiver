package remote_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	smithyhttp "github.com/aws/smithy-go/transport/http"

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

	client, fake := newClient(t, remote.Config{})

	body := []byte("sealed bundle bytes")
	err := client.Upload(t.Context(), "acme/bundles/logs.gen0001.zip",
		bytes.NewReader(body), int64(len(body)), "DEEP_ARCHIVE")
	require.NoError(t, err)

	obj, ok := fake.Object("acme/bundles/logs.gen0001.zip")
	require.True(t, ok, "object should be stored")
	assert.Equal(t, body, obj.Data)
	assert.Equal(t, "DEEP_ARCHIVE", obj.StorageClass, "the per-call storage class should be applied")
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

	err = client.Upload(t.Context(), "big.zip", bytes.NewReader(body), int64(len(body)), "")
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

	err := client.Upload(t.Context(), "k", bytes.NewReader([]byte("x")), 1, "")
	require.NoError(t, err)

	assert.Equal(t, []string{""}, fake.PutChecksums(), "checksums off should omit the checksum algorithm")
}

func TestUploadAbortsOnFailure(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{PartSize: manager.MinUploadPartSize})
	fake.UploadPartErr = errors.New("injected part failure")

	body := make([]byte, 2*manager.MinUploadPartSize)

	err := client.Upload(t.Context(), "big.zip", bytes.NewReader(body), int64(len(body)), "")
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

func TestHeadBareHTTPStatus(t *testing.T) {
	t.Parallel()

	// A nonconforming store may answer with a bodyless HTTP error the SDK
	// decodes to no error code at all; the status line is the only signal.
	bareStatus := func(code int) *smithyhttp.ResponseError {
		return &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: code}},
			Err:      errors.New("no error body"),
		}
	}

	tests := map[string]struct {
		status int
		err    error
	}{
		"a bodyless 404 classifies as not found": {
			status: http.StatusNotFound,
			err:    remote.ErrNotFound,
		},
		"a bodyless 403 stays an opaque error": {
			status: http.StatusForbidden,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, fake := newClient(t, remote.Config{})
			fake.HeadErr = bareStatus(tt.status)

			_, err := client.Head(t.Context(), "k")
			require.Error(t, err)

			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)
			} else {
				require.NotErrorIs(t, err, remote.ErrNotFound)
			}
		})
	}
}

func TestPut(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})

	body := []byte("loose search-layer file")
	require.NoError(t, client.Put(t.Context(), "acme/org.json", bytes.NewReader(body), "STANDARD_IA"))

	obj, ok := fake.Object("acme/org.json")
	require.True(t, ok, "object should be stored")
	assert.Equal(t, body, obj.Data)
	assert.Equal(t, "STANDARD_IA", obj.StorageClass, "the per-call storage class should be applied")
	assert.Equal(t, 1, fake.PutCalls(), "Put must stay a single request so the checksum is full-object")
	assert.Equal(t, 0, fake.Completed(), "Put never goes multipart")
	assert.Equal(t, []string{"SHA256"}, fake.PutChecksums())
	assert.NotEmpty(t, obj.ChecksumSHA256, "a checksummed Put records a full-object checksum")
	assert.NotContains(t, obj.ETag, "-", "a Put ETag is a plain MD5, never composite")
}

func TestPutDisableChecksums(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{DisableChecksums: true})

	require.NoError(t, client.Put(t.Context(), "k", bytes.NewReader([]byte("x")), ""))
	assert.Equal(t, []string{""}, fake.PutChecksums(), "checksums off should omit the checksum algorithm")

	obj, ok := fake.Object("k")
	require.True(t, ok)
	assert.Empty(t, obj.ChecksumSHA256)
}

func TestHeadChecksum(t *testing.T) {
	t.Parallel()

	// A full-object SHA-256 checksum decodes to exactly 32 raw bytes.
	digest := []byte("0123456789abcdef0123456789abcdef")
	wire := base64.StdEncoding.EncodeToString(digest)

	tests := map[string]struct {
		obj  remotetest.Object
		want []byte
	}{
		"full-object checksum decodes to raw digest bytes": {
			obj:  remotetest.Object{Data: []byte("ab"), ChecksumSHA256: wire},
			want: digest,
		},
		"composite checksum is blanked": {
			obj: remotetest.Object{Data: []byte("ab"), ChecksumSHA256: wire + "-3"},
		},
		"absent checksum stays nil": {
			obj: remotetest.Object{Data: []byte("ab")},
		},
		"undecodable checksum is blanked": {
			obj: remotetest.Object{Data: []byte("ab"), ChecksumSHA256: "not base64!"},
		},
		"decodable but wrong-length checksum is blanked": {
			// A value that base64-decodes cleanly but not to a 32-byte digest is
			// uninterpretable as a SHA-256, so it must not reach a caller that
			// would compare it against a locally computed digest.
			obj: remotetest.Object{
				Data:           []byte("ab"),
				ChecksumSHA256: base64.StdEncoding.EncodeToString([]byte("too short")),
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, fake := newClient(t, remote.Config{})
			fake.SetObject("k", tt.obj)

			info, err := client.Head(t.Context(), "k")
			require.NoError(t, err)
			assert.Equal(t, tt.want, info.SHA256)
			assert.Equal(t, []string{"ENABLED"}, fake.HeadChecksumModes(),
				"Head must request the checksum fields or the store omits them")
		})
	}
}

func TestList(t *testing.T) {
	t.Parallel()

	plainETag := remotetest.MD5Hex([]byte("abcd"))

	md5Sum, err := hex.DecodeString(plainETag)
	require.NoError(t, err)

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("hcp/acme/org.json", remotetest.Object{Data: []byte("abcd"), ETag: plainETag})
	fake.SetObject("hcp/acme/users/u1.json", remotetest.Object{Data: []byte("ab"), ETag: plainETag + "-2"})
	fake.SetObject("hcp/acme/rollups/r.json", remotetest.Object{Data: []byte("abc"), ETag: "opaque-etag"})
	fake.SetObject("hcp/other/org.json", remotetest.Object{Data: []byte("x")})

	got, err := client.List(t.Context(), "hcp/acme/")
	require.NoError(t, err)

	assert.Equal(t, map[string]remote.ListedObject{
		"hcp/acme/org.json":       {Size: 4, MD5: md5Sum},
		"hcp/acme/users/u1.json":  {Size: 2},
		"hcp/acme/rollups/r.json": {Size: 3},
	}, got, "only keys under the prefix list; only a plain single-part MD5 ETag yields a digest")
}

func TestListPaginates(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})

	// One past the fake's thousand-key page size forces a second page.
	for i := range 1001 {
		fake.SetObject(fmt.Sprintf("p/%04d", i), remotetest.Object{Data: []byte("x")})
	}

	got, err := client.List(t.Context(), "p/")
	require.NoError(t, err)

	assert.Len(t, got, 1001, "pagination should surface every key")
	assert.Equal(t, 2, fake.ListCalls())
}

func TestDelete(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("a", remotetest.Object{Data: []byte("x")})
	fake.SetObject("b", remotetest.Object{Data: []byte("y")})

	deleted, err := client.Delete(t.Context(), []string{"a", "b", "absent"})
	require.NoError(t, err)
	assert.Equal(t, 3, deleted, "every acknowledged key counts, including the no-op absent one")

	assert.Empty(t, fake.Keys(), "named keys should be removed")
	assert.Equal(t, []string{"a", "b", "absent"}, fake.Deleted(),
		"an absent key deletes as a no-op, per S3 semantics")
}

func TestDeleteBatches(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})

	// One past the thousand-key request ceiling forces a second batch.
	keys := make([]string, 1001)
	for i := range keys {
		keys[i] = fmt.Sprintf("k/%04d", i)
		fake.SetObject(keys[i], remotetest.Object{Data: []byte("x")})
	}

	deleted, err := client.Delete(t.Context(), keys)
	require.NoError(t, err)
	assert.Equal(t, 1001, deleted, "the count spans both batches")

	assert.Empty(t, fake.Keys(), "every key should be removed across batches")
	assert.Equal(t, 2, fake.DeleteCalls())
}

func TestDeleteEmpty(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.DeleteErr = errors.New("must not be called")

	deleted, err := client.Delete(t.Context(), nil)
	require.NoError(t, err, "no keys means no requests")
	assert.Zero(t, deleted)
}
