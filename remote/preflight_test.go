package remote_test

import (
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
		})
	}
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
