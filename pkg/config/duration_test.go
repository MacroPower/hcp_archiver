package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/config"
)

func TestParseDuration(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want time.Duration
	}{
		"days":                {in: "90d", want: 90 * 24 * time.Hour},
		"days compose":        {in: "1d12h", want: 36 * time.Hour},
		"fractional days":     {in: "1.5d", want: 36 * time.Hour},
		"plain go duration":   {in: "2160h", want: 2160 * time.Hour},
		"subsecond":           {in: "300ms", want: 300 * time.Millisecond},
		"single day":          {in: "1d", want: 24 * time.Hour},
		"repeated units sum":  {in: "1d1d", want: 48 * time.Hour},
		"micro sign spelling": {in: "5µs", want: 5 * time.Microsecond},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := config.ParseDuration(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, time.Duration(got))
		})
	}
}

func TestParseDuration_Invalid(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in string
	}{
		"missing unit":            {in: "90"},
		"unknown unit":            {in: "1w"},
		"unit without number":     {in: "d"},
		"signed":                  {in: "-1d"},
		"malformed number":        {in: "1.5.5d"},
		"empty":                   {in: ""},
		"bare zero":               {in: "0"},
		"overflowing segment":     {in: "10675200d"},
		"overflowing day scale":   {in: "1000000d"},
		"overflowing composition": {in: "106751d106751d"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := config.ParseDuration(tc.in)
			require.ErrorIs(t, err, config.ErrInvalidDuration)
		})
	}
}

func TestDuration_String(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   config.Duration
		want string
	}{
		"whole days":         {in: config.Duration(90 * 24 * time.Hour), want: "90d"},
		"single day":         {in: config.Duration(24 * time.Hour), want: "1d"},
		"fallback to go":     {in: config.Duration(36 * time.Hour), want: "36h0m0s"},
		"zero is not a day":  {in: config.Duration(0), want: "0s"},
		"subsecond fallback": {in: config.Duration(300 * time.Millisecond), want: "300ms"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, tc.in.String())
		})
	}
}

func TestParseDuration_RoundTrip(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"90d", "1d12h", "1.5d", "2160h", "300ms"} {
		parsed, err := config.ParseDuration(in)
		require.NoError(t, err)

		again, err := config.ParseDuration(parsed.String())
		require.NoError(t, err, "String() of %q must reparse", in)
		assert.Equal(t, parsed, again)
	}
}
