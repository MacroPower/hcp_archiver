package remote_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gocloud.dev/gcerrors"

	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/remote/remotetest"
)

// newClient builds a [*remote.Client] over a fresh fake store with in-client
// retrying off, so a test that injects a persistent fault observes exactly one
// attempt; retry behavior is exercised by the tests that opt back in through
// [newRetryClient].
func newClient(t *testing.T, cfg remote.Config) (*remote.Client, *remotetest.Fake) {
	t.Helper()

	fake := remotetest.New()

	client, err := remote.New(t.Context(), cfg,
		remote.WithBucket(fake.Bucket()), remote.WithRetry(0, 0))
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	return client, fake
}

// newRetryClient builds a [*remote.Client] over a fresh fake store with the
// given retry budget and no backoff delay.
func newRetryClient(t *testing.T, retries int) (*remote.Client, *remotetest.Fake) {
	t.Helper()

	fake := remotetest.New()

	client, err := remote.New(t.Context(), remote.Config{},
		remote.WithBucket(fake.Bucket()), remote.WithRetry(retries, 0))
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
		bytes.NewReader(body), int64(len(body)), remote.Digests{})
	require.NoError(t, err)

	obj, ok := fake.Object("acme/bundles/logs.gen0001.zip")
	require.True(t, ok, "object should be stored")
	assert.Equal(t, body, obj.Data)
	assert.Nil(t, obj.MD5, "a digest-less upload records none")
	assert.Nil(t, obj.Metadata, "a digest-less upload records no metadata")
	assert.Equal(t, 1, fake.PutCalls())
}

func TestUploadRecordsDigests(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})

	body := []byte("sealed bundle bytes")
	digests := remote.Digests{
		MD5:    remotetest.MD5Sum(body),
		SHA256: "6ff0f8bff0d0f81f34f4a7cbf7ba0e11e6c2a1c6a8a44e0f7e35c17e2c2a9d42",
	}

	err := client.Upload(t.Context(), "k", bytes.NewReader(body), int64(len(body)), digests)
	require.NoError(t, err)

	obj, ok := fake.Object("k")
	require.True(t, ok, "object should be stored")
	assert.Equal(t, body, obj.Data)
	assert.Equal(t, digests.MD5, obj.MD5, "the provided MD5 rides as the write's integrity check")
	assert.Equal(t, digests.SHA256, obj.Metadata["sha256"],
		"the SHA-256 lands in the object metadata for later egress-free comparison")

	info, err := client.Head(t.Context(), "k")
	require.NoError(t, err)
	assert.Equal(t, digests.SHA256, info.SHA256, "Head resolves the recorded metadata digest")
}

func TestUploadRejectsWrongMD5(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})

	err := client.Upload(t.Context(), "k", bytes.NewReader([]byte("body")), 4,
		remote.Digests{MD5: remotetest.MD5Sum([]byte("other"))})
	require.Error(t, err, "a body that does not hash to its declared MD5 must not commit")

	_, ok := fake.Object("k")
	assert.False(t, ok)
}

func TestHeadResolvesMetadataMD5(t *testing.T) {
	t.Parallel()

	// A parted upload records no backend MD5 attribute; the metadata digest
	// the upload recorded must still resolve through Head.
	body := []byte("parted body")
	digest := remotetest.MD5Sum(body)

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("k", remotetest.Object{
		Data:     body,
		Metadata: map[string]string{"md5": hex.EncodeToString(digest)},
	})

	info, err := client.Head(t.Context(), "k")
	require.NoError(t, err)
	assert.Equal(t, digest, info.MD5,
		"the metadata digest backfills an absent backend attribute")
}

func TestUploadLargeBody(t *testing.T) {
	t.Parallel()

	// A sub-minimum part size floors at S3's 5 MiB multipart minimum, and a
	// body spanning several floored parts forces the portable layer through
	// its buffered multi-flush path; the committed object must reassemble
	// byte for byte.
	client, fake := newClient(t, remote.Config{PartSize: 1 << 20})

	body := make([]byte, 2*(5<<20)+1024)
	_, err := rand.Read(body)
	require.NoError(t, err)

	err = client.Upload(t.Context(), "big.zip", bytes.NewReader(body), int64(len(body)), remote.Digests{})
	require.NoError(t, err)

	obj, ok := fake.Object("big.zip")
	require.True(t, ok, "object should be stored")
	assert.Equal(t, body, obj.Data)
}

// failingReader yields a little data and then a permanent error, modeling a
// body that dies mid-stream. Its Seek satisfies the upload's rewindable-body
// contract without clearing the failure.
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

func (r *failingReader) Seek(int64, int) (int64, error) {
	return 0, nil
}

func TestUploadAbortsOnBodyFailure(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})

	err := client.Upload(t.Context(), "big.zip", &failingReader{}, 4, remote.Digests{})
	require.Error(t, err)

	_, ok := fake.Object("big.zip")
	assert.False(t, ok, "a body that dies mid-stream must never commit a truncated object")
}

func TestUploadCommitFailure(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.PutErr = errors.New("injected commit failure")

	err := client.Upload(t.Context(), "k", bytes.NewReader([]byte("x")), 1, remote.Digests{})
	require.Error(t, err)

	_, ok := fake.Object("k")
	assert.False(t, ok, "a failed commit stores no object")
}

func TestUploadRetriesTransientCommitFailure(t *testing.T) {
	t.Parallel()

	client, fake := newRetryClient(t, 2)
	fake.PutErr = errors.New("injected transient commit failure")
	fake.PutErrN = 1

	body := []byte("sealed bundle bytes")
	err := client.Upload(t.Context(), "k", bytes.NewReader(body), int64(len(body)), remote.Digests{})
	require.NoError(t, err, "a fault that heals within the budget must not surface")

	obj, ok := fake.Object("k")
	require.True(t, ok)
	assert.Equal(t, body, obj.Data, "the retried attempt rewinds and re-streams the whole body")
	assert.Equal(t, 2, fake.PutCalls())
}

func TestRetryStopsAtBudget(t *testing.T) {
	t.Parallel()

	client, fake := newRetryClient(t, 2)
	fake.PutErr = errors.New("injected persistent failure")

	err := client.Put(t.Context(), "k", []byte("x"))
	require.Error(t, err, "a persistent fault surfaces once the budget is spent")
	assert.Equal(t, 3, fake.PutCalls(), "one attempt plus the configured retries")
}

func TestHeadRetriesTransientFailure(t *testing.T) {
	t.Parallel()

	client, fake := newRetryClient(t, 2)
	fake.SetObject("k", remotetest.Object{Data: []byte("abcd")})

	fake.HeadErr = errors.New("injected transient failure")
	fake.HeadErrN = 1

	info, err := client.Head(t.Context(), "k")
	require.NoError(t, err)
	assert.Equal(t, int64(4), info.Size)
}

func TestHeadNotFoundIsNeverRetried(t *testing.T) {
	t.Parallel()

	client, fake := newRetryClient(t, 3)

	_, err := client.Head(t.Context(), "missing")
	require.ErrorIs(t, err, remote.ErrNotFound)
	assert.Equal(t, 1, fake.HeadCalls(), "an absence settles on one response")
}

func TestListRetriesTransientFailure(t *testing.T) {
	t.Parallel()

	client, fake := newRetryClient(t, 2)
	fake.SetObject("p/a", remotetest.Object{Data: []byte("x")})

	fake.ListErr = errors.New("injected transient failure")
	fake.ListErrN = 1

	got, err := client.List(t.Context(), "p/")
	require.NoError(t, err)
	assert.Len(t, got, 1, "the retried listing is one whole enumeration")
}

func TestReadAtRetriesTransientFailure(t *testing.T) {
	t.Parallel()

	client, fake := newRetryClient(t, 2)
	fake.SetObject("k", remotetest.Object{Data: []byte("abcdef")})

	fake.RangeErr = errors.New("injected transient failure")
	fake.RangeErrN = 1

	p := make([]byte, 4)
	n, err := client.ReadAt(t.Context(), "k", 6, p, 1)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, []byte("bcde"), p, "the retried read refills the span from its offset")
}

func TestRetryStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	client, fake := newRetryClient(t, 3)
	fake.PutErr = errors.New("injected failure")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := client.Put(ctx, "k", []byte("x"))
	require.ErrorIs(t, err, context.Canceled,
		"a canceled context surfaces immediately instead of burning the retry budget")

	_, ok := fake.Object("k")
	assert.False(t, ok)
}

// TestRetryClassificationByCode pins the never-retry set: an error the store
// pins on the request itself surfaces after exactly one attempt, while a
// fault of the store or the path to it spends the whole retry budget. A
// refactor that drops a code from either side of the switch fails here
// rather than shipping invisibly.
func TestRetryClassificationByCode(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		code         gcerrors.ErrorCode
		wantAttempts int
	}{
		"permission denied surfaces immediately":   {code: gcerrors.PermissionDenied, wantAttempts: 1},
		"failed precondition surfaces immediately": {code: gcerrors.FailedPrecondition, wantAttempts: 1},
		"invalid argument surfaces immediately":    {code: gcerrors.InvalidArgument, wantAttempts: 1},
		"already exists surfaces immediately":      {code: gcerrors.AlreadyExists, wantAttempts: 1},
		"unimplemented surfaces immediately":       {code: gcerrors.Unimplemented, wantAttempts: 1},
		"canceled surfaces immediately":            {code: gcerrors.Canceled, wantAttempts: 1},
		"unknown retries":                          {code: gcerrors.Unknown, wantAttempts: 3},
		"internal retries":                         {code: gcerrors.Internal, wantAttempts: 3},
		"resource exhausted retries":               {code: gcerrors.ResourceExhausted, wantAttempts: 3},
		"deadline exceeded retries":                {code: gcerrors.DeadlineExceeded, wantAttempts: 3},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, fake := newRetryClient(t, 2)
			fake.PutErr = errors.New("injected classified failure")
			fake.ErrCode = tc.code

			err := client.Put(t.Context(), "k", []byte("x"))
			require.Error(t, err)
			assert.Equal(t, tc.wantAttempts, fake.PutCalls(),
				"the classification decides whether the budget is spent")
		})
	}
}

// TestTransportFaultNeverSettlesNotFound models azblob's quirk of mapping a
// "no such host" DNS failure to gcerrors.NotFound: a transport fault says
// nothing about the object at the key, so it must classify transient and
// never satisfy an absence check, whatever code the driver stamped on it.
func TestTransportFaultNeverSettlesNotFound(t *testing.T) {
	t.Parallel()

	dnsErr := &net.DNSError{Err: "no such host", Name: "account.blob.core.windows.net"}

	t.Run("head retries through the blip", func(t *testing.T) {
		t.Parallel()

		client, fake := newRetryClient(t, 2)
		fake.SetObject("k", remotetest.Object{Data: []byte("abcd")})

		fake.HeadErr = dnsErr
		fake.HeadErrN = 2
		fake.ErrCode = gcerrors.NotFound

		info, err := client.Head(t.Context(), "k")
		require.NoError(t, err, "a resolver blip must be retried, not settled as an absence")
		assert.Equal(t, int64(4), info.Size)
	})

	t.Run("head never answers ErrNotFound for a transport fault", func(t *testing.T) {
		t.Parallel()

		client, fake := newClient(t, remote.Config{})
		fake.SetObject("k", remotetest.Object{Data: []byte("abcd")})

		fake.HeadErr = dnsErr
		fake.ErrCode = gcerrors.NotFound

		_, err := client.Head(t.Context(), "k")
		require.Error(t, err)
		require.NotErrorIs(t, err, remote.ErrNotFound,
			"an eviction probe reading ErrNotFound here would re-upload or mis-settle")
	})

	t.Run("delete never counts a transport fault as removed", func(t *testing.T) {
		t.Parallel()

		client, fake := newClient(t, remote.Config{})
		fake.SetObject("k", remotetest.Object{Data: []byte("abcd")})

		fake.DeleteErr = dnsErr
		fake.ErrCode = gcerrors.NotFound

		deleted, err := client.Delete(t.Context(), []string{"k"})
		require.Error(t, err, "a delete that never reached the store must fail, not settle")
		assert.Zero(t, deleted)
	})
}

// TestStallWatchdogCutsWedgedAttempt proves a wedged connection costs one
// stall window, not a hung worker: the attempt is canceled, classifies
// transient, and the retry succeeds.
func TestStallWatchdogCutsWedgedAttempt(t *testing.T) {
	t.Parallel()

	fake := remotetest.New()

	client, err := remote.New(t.Context(), remote.Config{},
		remote.WithBucket(fake.Bucket()),
		remote.WithRetry(1, 0),
		remote.WithStallTimeout(30*time.Millisecond))
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	var wedged atomic.Bool

	wedged.Store(true)

	// The first commit blocks like a black-holed connection until its context
	// dies; only the watchdog can end it.
	fake.PutHook = func(ctx context.Context) {
		if wedged.Swap(false) {
			<-ctx.Done()
		}
	}

	err = client.Put(t.Context(), "k", []byte("x"))
	require.NoError(t, err, "a stalled attempt is cut and retried, not hung forever")

	obj, ok := fake.Object("k")
	require.True(t, ok)
	assert.Equal(t, []byte("x"), obj.Data)
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

func TestDeleteContinuesPastFailedKeys(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("a", remotetest.Object{Data: []byte("x")})
	fake.SetObject("b", remotetest.Object{Data: []byte("y")})
	fake.SetObject("c", remotetest.Object{Data: []byte("z")})

	fake.DeleteErr = errors.New("injected delete failure")
	fake.DeleteErrKeys = []string{"b"}

	deleted, err := client.Delete(t.Context(), []string{"a", "b", "c"})
	require.Error(t, err, "the failed key still surfaces")
	assert.Equal(t, 2, deleted, "one bad key must not strand the other stale keys")
	assert.ElementsMatch(t, []string{"a", "c"}, fake.Deleted())
}

func TestDeleteRetriesTransientFailure(t *testing.T) {
	t.Parallel()

	client, fake := newRetryClient(t, 2)
	fake.SetObject("a", remotetest.Object{Data: []byte("x")})

	fake.DeleteErr = errors.New("injected transient failure")
	fake.DeleteErrN = 1

	deleted, err := client.Delete(t.Context(), []string{"a"})
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)
	assert.Empty(t, fake.Keys())
}

func TestDeleteEmpty(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{})
	fake.DeleteErr = errors.New("must not be called")

	deleted, err := client.Delete(t.Context(), nil)
	require.NoError(t, err, "no keys means no requests")
	assert.Zero(t, deleted)
}

func TestCopyDuplicatesServerSide(t *testing.T) {
	t.Parallel()

	// The sweep's rename healing converges an only-copy onto its current key
	// without egress: the bytes and the recorded digest metadata must arrive
	// intact, and an absent source must classify as not-found so the caller
	// never releases an original behind a copy that did not happen.
	body := []byte("zip bytes")

	client, fake := newClient(t, remote.Config{})
	fake.SetObject("old/key", remotetest.Object{
		Data:     body,
		Metadata: map[string]string{"sha256": "abc"},
	})

	require.NoError(t, client.Copy(t.Context(), "old/key", "new/key"))

	obj, ok := fake.Object("new/key")
	require.True(t, ok)
	assert.Equal(t, body, obj.Data)
	assert.Equal(t, "abc", obj.Metadata["sha256"], "recorded digests ride along")
	assert.Equal(t, []remotetest.CopyRecord{{Src: "old/key", Dst: "new/key"}}, fake.Copies())

	err := client.Copy(t.Context(), "gone/key", "new/key")
	require.ErrorIs(t, err, remote.ErrNotFound)
}
