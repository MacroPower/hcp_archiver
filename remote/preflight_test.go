package remote_test

import (
	"context"
	"errors"
	"testing"

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

	err := client.Preflight(t.Context())
	require.ErrorIs(t, err, injected,
		"a store that cannot serve ranged reads must fail the preflight; view depends on them")
	assert.Contains(t, err.Error(), "ranged read")
}

func TestPreflightDigestMismatch(t *testing.T) {
	t.Parallel()

	client, fake := newClient(t, remote.Config{Prefix: "hcp"})

	// Corrupt the probe's recorded digest between the write and the metadata
	// read, modeling a store whose digests cannot be trusted; the sync gate
	// and eviction confirm compare these, so the run must not start.
	fake.HeadHook = func(context.Context) {
		fake.SetObject("hcp/.preflight", remotetest.Object{
			Data: []byte("hcp_archiver remote store probe\n"),
			MD5:  remotetest.MD5Sum([]byte("other bytes")),
		})
	}

	err := client.Preflight(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "digest")
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
