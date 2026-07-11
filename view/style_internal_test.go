package view

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		want string
		n    int64
	}{
		"zero bytes":                       {n: 0, want: "0 B"},
		"below one unit stays bytes":       {n: 1023, want: "1023 B"},
		"one kibibyte":                     {n: 1024, want: "1.0 KB"},
		"one and a half kibibytes":         {n: 1536, want: "1.5 KB"},
		"just under one mebibyte rolls up": {n: 1048570, want: "1.0 MB"},
		"one mebibyte":                     {n: 1 << 20, want: "1.0 MB"},
		"just under one gibibyte rolls up": {n: (1 << 30) - 1, want: "1.0 GB"},
		"two gibibytes":                    {n: 2 << 30, want: "2.0 GB"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, humanBytes(tc.n))
		})
	}
}
