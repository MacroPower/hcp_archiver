package view_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/view"
)

func TestDecodeResources(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		data string
		want int
		err  error
	}{
		"single object": {
			data: `{"data":{"id":"ws-1","type":"workspaces","attributes":{"name":"app"}}}`,
			want: 1,
		},
		"list": {
			data: `{"data":[{"id":"var-1","type":"vars"},{"id":"var-2","type":"vars"}]}`,
			want: 2,
		},
		"null payload": {
			data: `{"data":null}`,
			want: 0,
		},
		"empty list": {
			data: `{"data":[]}`,
			want: 0,
		},
		"malformed document": {
			data: `{"data":`,
			err:  view.ErrDecode,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			resources, err := view.DecodeResources([]byte(tt.data))
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)

				return
			}

			require.NoError(t, err)
			require.Len(t, resources, tt.want)
		})
	}
}

func TestResourceAccessors(t *testing.T) {
	t.Parallel()

	data := `{"data":{"id":"run-1","type":"runs","attributes":{` +
		`"status":"applied","serial":42,"has-changes":true,` +
		`"created-at":"2024-02-01T10:00:00Z","broken-time":"yesterday"}}}`

	resources, err := view.DecodeResources([]byte(data))
	require.NoError(t, err)
	require.Len(t, resources, 1)

	r := resources[0]

	require.Equal(t, "run-1", r.ID)
	require.Equal(t, "runs", r.Type)
	require.Equal(t, "applied", r.String("status"))
	require.Empty(t, r.String("missing"))
	require.EqualValues(t, 42, r.Int("serial"))
	require.Zero(t, r.Int("missing"))
	require.True(t, r.Bool("has-changes"))
	require.False(t, r.Bool("missing"))
	require.Equal(t, time.Date(2024, 2, 1, 10, 0, 0, 0, time.UTC), r.Time("created-at"))
	require.True(t, r.Time("broken-time").IsZero())
}
