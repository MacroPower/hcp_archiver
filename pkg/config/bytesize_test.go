package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/config"
)

func TestParseByteSize(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want int64
	}{
		"binary suffix":       {in: "64MiB", want: 67108864},
		"decimal kilobyte":    {in: "1KB", want: 1000},
		"binary kibibyte":     {in: "1KiB", want: 1024},
		"fractional binary":   {in: "1.5GiB", want: 1610612736},
		"fractional decimal":  {in: "1.5MB", want: 1500000},
		"fractional is exact": {in: "1.1KB", want: 1100},
		"plain integer":       {in: "123", want: 123},
		"single byte":         {in: "1B", want: 1},
		"zero":                {in: "0", want: 0},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := config.ParseByteSize(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, int64(got))
		})
	}
}

func TestParseByteSize_Invalid(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in string
	}{
		"lowercase suffix":  {in: "64mib"},
		"embedded space":    {in: "64 MiB"},
		"bare suffix":       {in: "MiB"},
		"signed":            {in: "-1MiB"},
		"fractional byte":   {in: "1.5B"},
		"bare fractional":   {in: "1.5"},
		"unknown suffix":    {in: "1EB"},
		"empty":             {in: ""},
		"overflowing value": {in: "9000000TiB"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := config.ParseByteSize(tc.in)
			require.ErrorIs(t, err, config.ErrInvalidByteSize)
		})
	}
}

func TestByteSize_String(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   config.ByteSize
		want string
	}{
		"exact binary unit":  {in: config.ByteSize(67108864), want: "64MiB"},
		"largest unit wins":  {in: config.ByteSize(1024 * 1024), want: "1MiB"},
		"decimal falls back": {in: config.ByteSize(1000), want: "1000"},
		"zero":               {in: config.ByteSize(0), want: "0"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, tc.in.String())
		})
	}
}

func TestParseByteSize_RoundTrip(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"64MiB", "1KB", "1.5GiB", "123"} {
		parsed, err := config.ParseByteSize(in)
		require.NoError(t, err)

		again, err := config.ParseByteSize(parsed.String())
		require.NoError(t, err, "String() of %q must reparse", in)
		assert.Equal(t, parsed, again)
	}
}
