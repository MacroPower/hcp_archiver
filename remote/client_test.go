package remote_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/remote/remotetest"
)

// newClient builds a [*remote.Client] over a fresh fake store.
func newClient(t *testing.T, cfg remote.Config) (*remote.Client, *remotetest.Fake) {
	t.Helper()

	fake := remotetest.New()

	client, err := remote.New(t.Context(), cfg, remote.WithBucket(fake.Bucket()))
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	return client, fake
}

func TestNewMissingURL(t *testing.T) {
	t.Parallel()

	_, err := remote.New(t.Context(), remote.Config{})
	require.ErrorIs(t, err, remote.ErrMissingURL)
}

func TestUpload(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})

	body := []byte("sealed bundle bytes")
	err := client.Upload(t.Context(), "acme/bundles/logs.gen0001.zip",
		bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)

	obj, ok := fake.Object("acme/bundles/logs.gen0001.zip")
	require.True(t, ok, "object should be stored")
	assert.Equal(t, body, obj.Data)
	assert.Nil(t, obj.MD5, "an upload records no digest; eviction's gate is existence and size")
	assert.Equal(t, 1, fake.PutCalls())
}

func TestUploadLargeBody(t *testing.T) {
	t.Parallel()

	// A part size far below the body forces the portable layer through its
	// buffered multi-flush path; the committed object must reassemble byte
	// for byte.
	client, fake := newClient(t, remote.Config{PartSize: 1 << 20})

	body := make([]byte, 2<<20+1024)
	_, err := rand.Read(body)
	require.NoError(t, err)

	err = client.Upload(t.Context(), "big.zip", bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)

	obj, ok := fake.Object("big.zip")
	require.True(t, ok, "object should be stored")
	assert.Equal(t, body, obj.Data)
}

// failingReader yields a little data and then a permanent error, modeling a
// body that dies mid-stream.
type failingReader struct {
	served bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.served {
		r.served = true
		p[0] = 'x'

		return 1, nil
	}

	return 0, errors.New("injected body failure")
}

func TestUploadAbortsOnBodyFailure(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})

	err := client.Upload(t.Context(), "big.zip", &failingReader{}, 4)
	require.Error(t, err)

	_, ok := fake.Object("big.zip")
	assert.False(t, ok, "a body that dies mid-stream must never commit a truncated object")
}

func TestUploadCommitFailure(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.PutErr = errors.New("injected commit failure")

	err := client.Upload(t.Context(), "k", bytes.NewReader([]byte("x")), 1)
	require.Error(t, err)

	_, ok := fake.Object("k")
	assert.False(t, ok, "a failed commit stores no object")
}

func TestHead(t *testing.T) {
	t.Parallel()

	digest := remotetest.MD5Sum([]byte("abcd"))

	tests := map[string]struct {
		obj  *remotetest.Object
		key  string
		want remote.ObjectInfo
		err  error
	}{
		"present with digest": {
			obj:  &remotetest.Object{Data: []byte("abcd"), MD5: digest},
			key:  "k",
			want: remote.ObjectInfo{Size: 4, MD5: digest},
		},
		"present without digest": {
			obj:  &remotetest.Object{Data: []byte("ab")},
			key:  "k",
			want: remote.ObjectInfo{Size: 2},
		},
		"absent": {
			key: "missing",
			err: remote.ErrNotFound,
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

func TestHeadOpaqueError(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.HeadErr = errors.New("injected store failure")

	_, err := client.Head(t.Context(), "k")
	require.Error(t, err)
	require.NotErrorIs(t, err, remote.ErrNotFound,
		"only a store-classified not-found maps to ErrNotFound")
}

func TestPut(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})

	body := []byte("loose search-layer file")
	require.NoError(t, client.Put(t.Context(), "acme/org.json", body))

	obj, ok := fake.Object("acme/org.json")
	require.True(t, ok, "object should be stored")
	assert.Equal(t, body, obj.Data)
	assert.Equal(t, remotetest.MD5Sum(body), obj.MD5,
		"a Put records the full-object digest later sweeps compare")
	assert.Equal(t, 1, fake.PutCalls())
}

func TestList(t *testing.T) {
	t.Parallel()

	digest := remotetest.MD5Sum([]byte("abcd"))

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("hcp/acme/org.json", remotetest.Object{Data: []byte("abcd"), MD5: digest})
	fake.SetObject("hcp/acme/users/u1.json", remotetest.Object{Data: []byte("ab")})
	fake.SetObject("hcp/other/org.json", remotetest.Object{Data: []byte("x")})

	got, err := client.List(t.Context(), "hcp/acme/")
	require.NoError(t, err)

	assert.Equal(t, map[string]remote.ObjectInfo{
		"hcp/acme/org.json":      {Size: 4, MD5: digest},
		"hcp/acme/users/u1.json": {Size: 2},
	}, got, "only keys under the prefix list; a digest surfaces only when the store recorded one")
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
	assert.Equal(t, 3, deleted, "every settled key counts, including the no-op absent one")

	assert.Empty(t, fake.Keys(), "named keys should be removed")
	assert.ElementsMatch(t, []string{"a", "b"}, fake.Deleted(),
		"the fan-out settles the named keys in no particular order")
}

func TestDeletePartialFailure(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("a", remotetest.Object{Data: []byte("x")})

	fake.DeleteErr = errors.New("injected delete failure")

	deleted, err := client.Delete(t.Context(), []string{"a"})
	require.Error(t, err)
	assert.Zero(t, deleted, "a failed delete must not count as removed")
}

func TestDeleteEmpty(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.DeleteErr = errors.New("must not be called")

	deleted, err := client.Delete(t.Context(), nil)
	require.NoError(t, err, "no keys means no requests")
	assert.Zero(t, deleted)
}
