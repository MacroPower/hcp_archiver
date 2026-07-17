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

func TestFlushAppendsCrossShardOwnersFirst(t *testing.T) {
	t.Parallel()

	// A flush whose batch spans the config-versions shard and a workspace shard
	// must make the config-versions delta durable first: a crash after the
	// workspace append but before the config-versions one would durably freeze
	// a run behind its settled collection while losing the done entry of the
	// tarball it references, and no later run re-records a tarball below the
	// walk's early-stop boundary. An injected append failure on the workspace
	// shard stands in for the crash; with a map-random append order each
	// iteration could see the workspace shard drawn first, so the loop makes a
	// regression overwhelmingly likely to surface. The workspace entry is
	// recorded before the tarball so a map iteration that merely follows
	// insertion order still puts the workspace shard first without the sort.
	for range 8 {
		root := t.TempDir()

		l, err := Load(root)
		require.NoError(t, err)

		t.Cleanup(func() { require.NoError(t, l.Close()) })

		const (
			tarball = "config-versions/cv-1.tar.gz"
			wsRun   = "projects/p1/workspaces/w1/runs/run-1/run.json"
		)

		l.RecordDone(wsRun, Signature{})
		l.RecordDone(tarball, Signature{})

		// Occupy the workspace shard's log path with a directory so its append
		// fails while the config-versions shard's append can still succeed.
		wsLog := l.shardFor(wsRun).logPath()
		require.NoError(t, os.MkdirAll(wsLog, 0o750))

		require.Error(t, l.Flush())

		data, err := os.ReadFile(l.shardFor(tarball).logPath())
		require.NoError(t, err, "the config-versions delta must be durable before the workspace append")
		require.Contains(t, string(data), "cv-1.tar.gz")
	}
}
