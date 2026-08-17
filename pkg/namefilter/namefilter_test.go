package namefilter_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.jacobcolvin.com/hcp_archiver/pkg/namefilter"
)

func TestNew(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		names []string
		want  int
	}{
		"nil names build no filter":   {names: nil, want: 0},
		"empty names build no filter": {names: []string{}, want: 0},
		"one name":                    {names: []string{"alpha"}, want: 1},
		"duplicates collapse":         {names: []string{"alpha", "alpha"}, want: 1},
		"distinct names":              {names: []string{"alpha", "beta"}, want: 2},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := namefilter.New(tc.names)

			assert.Len(t, f, tc.want)

			if tc.want == 0 {
				assert.Nil(t, f, "an empty list yields a nil filter, which admits everything")
			}
		})
	}
}

func TestFilterAdmits(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		names []string
		name  string
		want  bool
	}{
		"nil filter admits any name":        {names: nil, name: "alpha", want: true},
		"nil filter admits the empty name":  {names: nil, name: "", want: true},
		"listed name admits":                {names: []string{"alpha", "beta"}, name: "beta", want: true},
		"unlisted name rejects":             {names: []string{"alpha"}, name: "beta", want: false},
		"empty name rejects under a filter": {names: []string{"alpha"}, name: "", want: false},
		"match is exact, not a prefix":      {names: []string{"alpha"}, name: "alpha-two", want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, namefilter.New(tc.names).Admits(tc.name))
		})
	}
}
