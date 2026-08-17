package theme_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.jacobcolvin.com/hcp_archiver/pkg/theme"
)

func TestCountNoun(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		want string
		n    int
	}{
		"zero takes the plural":  {n: 0, want: "0 files"},
		"one takes the singular": {n: 1, want: "1 file"},
		"many take the plural":   {n: 42, want: "42 files"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, theme.CountNoun(tc.n, "file", "files"))
		})
	}
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		want string
		n    int64
	}{
		"zero bytes":                       {n: 0, want: "0 B"},
		"below one unit stays bytes":       {n: 1023, want: "1023 B"},
		"one kibibyte":                     {n: 1024, want: "1.0 KiB"},
		"one and a half kibibytes":         {n: 1536, want: "1.5 KiB"},
		"just under one mebibyte rolls up": {n: 1048570, want: "1.0 MiB"},
		"one mebibyte":                     {n: 1 << 20, want: "1.0 MiB"},
		"just under one gibibyte rolls up": {n: (1 << 30) - 1, want: "1.0 GiB"},
		"two gibibytes":                    {n: 2 << 30, want: "2.0 GiB"},
		"one tebibyte":                     {n: 1 << 40, want: "1.0 TiB"},
		"largest count saturates at EiB":   {n: math.MaxInt64, want: "8.0 EiB"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, theme.HumanBytes(tc.n))
		})
	}
}
