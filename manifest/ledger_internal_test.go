package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiscoverShardsWithGlobMetaInRoot(t *testing.T) {
	t.Parallel()

	// A root whose own name carries a glob metacharacter: a literal '[' that a
	// glob would read as a character class and never match, silently hiding every
	// shard and re-archiving the organization from empty.
	root := filepath.Join(t.TempDir(), "arch[iv]e")

	for _, dir := range []string{
		filepath.Join(root, LedgerDirName),
		filepath.Join(root, "config-versions", LedgerDirName),
		filepath.Join(root, "projects", "p1", "workspaces", "w1", LedgerDirName),
		filepath.Join(root, "projects", "p1", "stacks", "s1", LedgerDirName),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o750))
	}

	got, err := discoverShards(root)
	require.NoError(t, err)

	gotKeys := make(map[string]struct{}, len(got))
	for sk := range got {
		gotKeys[sk] = struct{}{}
	}

	require.Equal(t, map[string]struct{}{
		"":                          {},
		"config-versions":           {},
		"projects/p1/workspaces/w1": {},
		"projects/p1/stacks/s1":     {},
	}, gotKeys)
}
