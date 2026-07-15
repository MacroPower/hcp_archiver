package remote_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/remote/remotetest"
)

func TestPreflight(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		prefix string
		want   string
	}{
		"with prefix": {prefix: "hcp", want: "hcp/.preflight"},
		"no prefix":   {prefix: "", want: ".preflight"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, fake := newClient(t, remote.Config{Prefix: tc.prefix})

			err := client.Preflight(t.Context())
			require.NoError(t, err)

			assert.Empty(t, fake.Keys(), "the probe should not outlive the preflight")
			assert.Equal(t, []string{tc.want}, fake.Deleted(),
				"the probe should compose under the prefix, beside any organization's subtree")
			assert.Equal(t, 1, fake.PutCalls())
			assert.Equal(t, 1, fake.HeadCalls())
			assert.Equal(t, 1, fake.ListCalls())
			assert.Equal(t, []string{"SHA256"}, fake.PutChecksums(),
				"the probe write should carry the same checksum settings as real uploads")
		})
	}
}

func TestPreflightDisableChecksums(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{DisableChecksums: true})

	err := client.Preflight(t.Context())
	require.NoError(t, err)

	assert.Equal(t, []string{""}, fake.PutChecksums(), "checksums off should omit the checksum algorithm")
	assert.Equal(t, []string{""}, fake.HeadChecksumModes(), "checksums off should omit the checksum mode")
}

func TestPreflightStoreErrors(t *testing.T) {
	t.Parallel()

	injected := errors.New("injected fault")

	tests := map[string]struct {
		inject func(*remotetest.Fake)
		want   string
	}{
		"put":    {inject: func(f *remotetest.Fake) { f.PutErr = injected }, want: "put"},
		"head":   {inject: func(f *remotetest.Fake) { f.HeadErr = injected }, want: "head"},
		"list":   {inject: func(f *remotetest.Fake) { f.ListErr = injected }, want: "list"},
		"delete": {inject: func(f *remotetest.Fake) { f.DeleteErr = injected }, want: "delete"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, fake := newClient(t, remote.Config{Prefix: "hcp"})
			tc.inject(fake)

			err := client.Preflight(t.Context())
			require.ErrorIs(t, err, injected)
			assert.Contains(t, err.Error(), "preflight")
			assert.Contains(t, err.Error(), tc.want, "the message should name the motion that surfaced the fault")
		})
	}
}

func TestPreflightWrongBucket(t *testing.T) {
	t.Parallel()

	fake := remotetest.New("other-bucket")

	client, err := remote.New(t.Context(), remote.Config{Bucket: testBucket}, remote.WithS3API(fake))
	require.NoError(t, err)

	err = client.Preflight(t.Context())
	require.Error(t, err, "a bucket the store does not serve should surface at preflight")
}

// wrongSizeAPI serves a probe whose metadata reports one byte more than was
// written, modeling a nonconforming store.
type wrongSizeAPI struct {
	*remotetest.Fake
}

func (w wrongSizeAPI) HeadObject(
	ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options),
) (*s3.HeadObjectOutput, error) {
	out, err := w.Fake.HeadObject(ctx, in, opts...)
	if err != nil {
		return nil, err //nolint:wrapcheck // A transparent wrapper over the fake.
	}

	out.ContentLength = aws.Int64(aws.ToInt64(out.ContentLength) + 1)

	return out, nil
}

func TestPreflightSizeMismatch(t *testing.T) {
	t.Parallel()

	api := wrongSizeAPI{Fake: remotetest.New(testBucket)}

	client, err := remote.New(t.Context(), remote.Config{Bucket: testBucket}, remote.WithS3API(api))
	require.NoError(t, err)

	err = client.Preflight(t.Context())
	require.ErrorContains(t, err, "reads back")
}

// emptyListAPI serves listings that omit every object, modeling a store whose
// writes do not surface in its listings.
type emptyListAPI struct {
	*remotetest.Fake
}

func (e emptyListAPI) ListObjectsV2(
	_ context.Context, _ *s3.ListObjectsV2Input, _ ...func(*s3.Options),
) (*s3.ListObjectsV2Output, error) {
	return &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}, nil
}

func TestPreflightMissingFromListing(t *testing.T) {
	t.Parallel()

	api := emptyListAPI{Fake: remotetest.New(testBucket)}

	client, err := remote.New(t.Context(), remote.Config{Bucket: testBucket}, remote.WithS3API(api))
	require.NoError(t, err)

	err = client.Preflight(t.Context())
	require.ErrorContains(t, err, "missing from its own listing")
}
