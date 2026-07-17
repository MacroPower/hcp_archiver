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

func TestDiscoverShardsFollowsSymlinkedDirectories(t *testing.T) {
	t.Parallel()

	// An operator can relocate a large subtree to another volume and leave a
	// symlink in its place. Discovery must follow the link: skipping it hides
	// every shard beneath, re-archives the subtree from empty, and the next
	// compaction overwrites the pre-existing snapshots.
	tmp := t.TempDir()
	root := filepath.Join(tmp, "archive")
	relocated := filepath.Join(tmp, "relocated")

	for _, dir := range []string{
		filepath.Join(root, LedgerDirName),
		filepath.Join(relocated, "p1", "workspaces", "w1", LedgerDirName),
		filepath.Join(relocated, "p2-target", "stacks", "s1", LedgerDirName),
		filepath.Join(relocated, "p3", "workspaces"),
		filepath.Join(tmp, "w3-target", LedgerDirName),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o750))
	}

	// The whole projects level lives behind one symlink, a single project is a
	// symlink itself, and a workspace directory is a symlinked leaf.
	require.NoError(t, os.Symlink(relocated, filepath.Join(root, "projects")))
	require.NoError(t, os.Symlink(
		filepath.Join(relocated, "p2-target"),
		filepath.Join(relocated, "p2"),
	))
	require.NoError(t, os.Symlink(
		filepath.Join(tmp, "w3-target"),
		filepath.Join(relocated, "p3", "workspaces", "w3"),
	))

	// A leaf alias of another project's workspace: the same physical .ledger
	// as p1/w1, reachable under a second key.
	require.NoError(t, os.Symlink(
		filepath.Join(relocated, "p1", "workspaces", "w1"),
		filepath.Join(relocated, "p3", "workspaces", "w4"),
	))

	// A dangling symlink and a symlink to a file are skipped, not errors.
	require.NoError(t, os.Symlink(
		filepath.Join(tmp, "gone"),
		filepath.Join(relocated, "dangling"),
	))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "note.txt"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(
		filepath.Join(tmp, "note.txt"),
		filepath.Join(relocated, "not-a-dir"),
	))

	got, err := discoverShards(root)
	require.NoError(t, err)

	gotKeys := make(map[string]struct{}, len(got))
	for sk := range got {
		gotKeys[sk] = struct{}{}
	}

	// Every reachable key surfaces, aliases included: the p2 alias beside its
	// p2-target and the w4 alias of p1/w1 each appear under both keys, so a
	// lookup through either resolves instead of re-fetching the subtree every
	// run. Collapsing the aliases onto one shard is shardAt's job, keyed by
	// physical identity, verified below through Load.
	require.Equal(t, map[string]struct{}{
		"":                             {},
		"projects/p1/workspaces/w1":    {},
		"projects/p2/stacks/s1":        {},
		"projects/p2-target/stacks/s1": {},
		"projects/p3/workspaces/w3":    {},
		"projects/p3/workspaces/w4":    {},
	}, gotKeys)

	l, err := Load(root)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, l.Close()) })

	// Both alias keys resolve to one shard over the one physical directory.
	require.Same(t, l.shards["projects/p2/stacks/s1"], l.shards["projects/p2-target/stacks/s1"])
	require.Same(t, l.shards["projects/p1/workspaces/w1"], l.shards["projects/p3/workspaces/w4"])
	require.Len(t, l.physShards, 4)
}

func TestFlushAppendsOneClassOrderedBatch(t *testing.T) {
	t.Parallel()

	// A flush spanning several shards lands as one batch in the single
	// org-level log — one fsync domain, so a crash tears a prefix of the whole
	// batch rather than between shards — and the batch is class-ordered: every
	// entry precedes every collection settlement, so a tarball proof in one
	// shard is always durable before the settlement that could freeze its
	// referencing run in another.
	root := t.TempDir()

	l, err := Load(root)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, l.Close()) })

	const (
		tarball = "config-versions/cv-1.tar.gz"
		prefix  = "projects/p1/workspaces/w1/runs"
		wsRun   = prefix + "/run-1/run.json"
	)

	l.RecordDone(wsRun, Signature{})
	l.RecordDone(tarball, Signature{})
	l.Collection(prefix).MarkComplete()
	l.Collection(prefix).SetSettled(true)

	require.NoError(t, l.Flush())

	_, err = os.Stat(l.shardFor(wsRun).logPath())
	require.ErrorIs(t, err, os.ErrNotExist, "no per-shard log is written")

	data, err := os.ReadFile(l.walPath())
	require.NoError(t, err)

	recs, _, err := replayLog(l.walPath(), l.logger)
	require.NoError(t, err)
	require.Len(t, recs, 4)

	lastClass := 0

	for i := range recs {
		class := walRecordClass(&recs[i])
		require.GreaterOrEqual(t, class, lastClass, "batch classes are non-decreasing: %s", string(data))

		lastClass = class
	}

	require.Equal(t, walEntry, recs[0].Kind)
	require.Equal(t, walSettled, recs[3].Kind, "the settlement trails every entry")
	require.Equal(t, "projects/p1/workspaces/w1", recs[len(recs)-1].Shard,
		"records carry the shard tag replay routes by")
}

func TestEntryDurableHoldsThroughTheDrainWindow(t *testing.T) {
	t.Parallel()

	// Between a flush's drain and the acknowledged append, an entry is in
	// neither the dirty set nor the log: durability must read false through
	// that window, or a concurrent custody check could authorize destroying
	// local bytes whose record a failed append then takes back.
	root := t.TempDir()

	l, err := Load(root)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, l.Close()) })

	const key = "config-versions/cv-1.tar.gz"

	l.RecordDone(key, Signature{Size: 1})
	require.False(t, l.EntryDurable(key), "recorded but unflushed")

	l.mu.Lock()

	sh, ok := l.lookupShard(key)
	require.True(t, ok)

	d := sh.drainDirty()

	l.mu.Unlock()

	require.False(t, l.EntryDurable(key), "drained but unacknowledged stays non-durable")

	// A failed append hands the drain back; the entry is dirty again.
	l.mu.Lock()
	sh.restoreDirty(d)
	l.mu.Unlock()

	require.False(t, l.EntryDurable(key))

	require.NoError(t, l.Flush())
	require.True(t, l.EntryDurable(key), "the acknowledged append closes the window")
}
