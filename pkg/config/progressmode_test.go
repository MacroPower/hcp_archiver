package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/config"
)

func TestParseProgressMode(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in      string
		want    config.ProgressMode
		wantErr error
	}{
		"auto":    {in: "auto", want: config.ProgressModeAuto},
		"human":   {in: "human", want: config.ProgressModeHuman},
		"json":    {in: "json", want: config.ProgressModeJSON},
		"quiet":   {in: "quiet", want: config.ProgressModeQuiet},
		"unknown": {in: "loud", wantErr: config.ErrInvalidProgressMode},
		"empty":   {in: "", wantErr: config.ErrInvalidProgressMode},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := config.ParseProgressMode(tc.in)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			// Round-trip: String of the parsed mode re-parses to itself.
			assert.Equal(t, tc.in, got.String())
		})
	}
}

func TestProgressModeString(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mode config.ProgressMode
		want string
	}{
		"auto":  {mode: config.ProgressModeAuto, want: "auto"},
		"human": {mode: config.ProgressModeHuman, want: "human"},
		"json":  {mode: config.ProgressModeJSON, want: "json"},
		"quiet": {mode: config.ProgressModeQuiet, want: "quiet"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, tc.mode.String())

			got, err := config.ParseProgressMode(tc.mode.String())
			require.NoError(t, err)
			assert.Equal(t, tc.mode, got)
		})
	}
}
