package collect_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
)

func TestAliasRewrites(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		aliases map[string]string
		relPath string
		want    []string
	}{
		"no alias spells no historical key": {
			aliases: map[string]string{},
			relPath: "projects/prod/workspaces/api/bundles/logs.gen0001.zip",
			want:    nil,
		},
		"an unrelated alias does not apply": {
			aliases: map[string]string{"projects/old": "projects/other"},
			relPath: "projects/prod/workspaces/api/bundles/logs.gen0001.zip",
			want:    nil,
		},
		"one rename spells the pre-rename key": {
			aliases: map[string]string{"projects/old": "projects/prod"},
			relPath: "projects/prod/workspaces/api/bundles/logs.gen0001.zip",
			want: []string{
				"projects/old/workspaces/api/bundles/logs.gen0001.zip",
			},
		},
		"stacked renames compose into the both-old-names key": {
			aliases: map[string]string{
				"projects/old":                 "projects/prod",
				"projects/prod/workspaces/www": "projects/prod/workspaces/api",
			},
			relPath: "projects/prod/workspaces/api/bundles/logs.gen0001.zip",
			want: []string{
				"projects/old/workspaces/api/bundles/logs.gen0001.zip",
				"projects/prod/workspaces/www/bundles/logs.gen0001.zip",
				"projects/old/workspaces/www/bundles/logs.gen0001.zip",
			},
		},
		// An alias nested beneath its own owner re-exposes the owner prefix in
		// every substitution's result, so each pass mints a strictly longer,
		// never-before-seen candidate. The seen set cannot bound that; only the
		// round cap can, and without it this case never returns.
		"an alias beneath its own owner terminates": {
			aliases: map[string]string{"projects/prod/link": "projects/prod"},
			relPath: "projects/prod/workspaces/api/bundles/logs.gen0001.zip",
			want: []string{
				"projects/prod/link/workspaces/api/bundles/logs.gen0001.zip",
			},
		},
		"mutually nested aliases terminate": {
			aliases: map[string]string{
				"projects/a/link": "projects/b",
				"projects/b/link": "projects/a",
			},
			relPath: "projects/a/workspaces/api/bundles/logs.gen0001.zip",
			want: []string{
				"projects/b/link/workspaces/api/bundles/logs.gen0001.zip",
				"projects/a/link/link/workspaces/api/bundles/logs.gen0001.zip",
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, collect.AliasRewrites(tc.aliases, tc.relPath))
		})
	}
}
