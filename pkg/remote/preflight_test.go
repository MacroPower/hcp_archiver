package remote_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote/remotetest"
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

			report, err := client.Preflight(t.Context())
			require.NoError(t, err)
			assert.False(t, report.AttrDigestsUntrusted,
				"a store whose attribute digest matches the written bytes stays trusted")

			assert.Empty(t, fake.Keys(), "the probe should not outlive the preflight")
			assert.Equal(t, []string{tc.want}, fake.Deleted(),
				"the probe should compose under the prefix, beside any organization's subtree")
			assert.Equal(t, 1, fake.PutCalls())
			assert.Equal(t, 1, fake.HeadCalls())
			assert.Equal(t, 1, fake.ListCalls())
			assert.Equal(t, []remotetest.Range{{Offset: 4, Length: 28}}, fake.Ranges(),
				"the probe must exercise a ranged read from a non-zero offset")
		})
	}
}

func TestPreflightRangedReadFault(t *testing.T) {
	t.Parallel()

	injected := errors.New("injected fault")

	client, fake := newClient(t, remote.Config{Prefix: "hcp"})
	fake.RangeErr = injected

	_, err := client.Preflight(t.Context())
	require.ErrorIs(t, err, injected,
		"a store that cannot serve ranged reads must fail the preflight; view depends on them")
	assert.Contains(t, err.Error(), "ranged read")
}

func TestPreflightMetadataDropped(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{Prefix: "hcp"})

	// Strip the probe's recorded metadata between the write and the read,
	// modeling a store that does not persist object metadata. The eviction
	// confirm and the incremental sync gate compare metadata digests, so such
	// a store would silently degrade the custody transfer to size-only; the
	// run must not start.
	fake.HeadHook = func(context.Context) {
		obj, ok := fake.Object("hcp/.preflight")
		if ok {
			obj.Metadata = nil
			fake.SetObject("hcp/.preflight", obj)
		}
	}

	_, err := client.Preflight(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata")
}

func TestPreflightUntrustedAttributeDigest(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{Prefix: "hcp"})

	// Replace the probe's backend digest attribute with one that hashes
	// nothing readable while its recorded metadata stays intact (the shape of
	// an SSE-KMS-encrypted S3 bucket, whose ETags are hex but not content
	// MD5s). The run proceeds; the client just stops serving attribute digests.
	fake.HeadHook = func(context.Context) {
		obj, ok := fake.Object("hcp/.preflight")
		if ok {
			obj.MD5 = remotetest.MD5Sum([]byte("other bytes"))
			fake.SetObject("hcp/.preflight", obj)
		}
	}

	report, err := client.Preflight(t.Context())
	require.NoError(t, err,
		"a bogus backend attribute must downgrade to metadata-only digests, not fail the run")
	assert.True(t, report.AttrDigestsUntrusted)

	// The downgrade must hold for the rest of the run: an object recorded
	// without metadata (a foreign write) now serves no digest at all rather
	// than a value that is no content MD5.
	fake.HeadHook = nil
	fake.SetObject("hcp/foreign.json", remotetest.Object{
		Data: []byte("{}"),
		MD5:  remotetest.MD5Sum([]byte("other bytes")),
	})

	info, err := client.Head(t.Context(), "hcp/foreign.json")
	require.NoError(t, err)
	assert.Nil(t, info.MD5, "an untrusted attribute digest must not be served")

	listed, err := client.List(t.Context(), "hcp/")
	require.NoError(t, err)
	require.Contains(t, listed, "hcp/foreign.json")
	assert.Nil(t, listed["hcp/foreign.json"].MD5,
		"listings serve only the attribute, so a distrusted one leaves the entry digestless")
}

func TestPreflightStoreErrors(t *testing.T) {
	t.Parallel()

	injected := errors.New("injected fault")

	tests := map[string]struct {
		inject func(*remotetest.Fake)
		want   string
	}{
		"upload": {inject: func(f *remotetest.Fake) { f.PutErr = injected }, want: "upload"},
		"head":   {inject: func(f *remotetest.Fake) { f.HeadErr = injected }, want: "head"},
		"list":   {inject: func(f *remotetest.Fake) { f.ListErr = injected }, want: "list"},
		"delete": {inject: func(f *remotetest.Fake) { f.DeleteErr = injected }, want: "delete"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, fake := newClient(t, remote.Config{Prefix: "hcp"})
			tc.inject(fake)

			_, err := client.Preflight(t.Context())
			require.ErrorIs(t, err, injected)
			assert.Contains(t, err.Error(), "preflight")
			assert.Contains(t, err.Error(), tc.want, "the message should name the motion that surfaced the fault")
		})
	}
}
