package fsid_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/fsid"
)

func TestCanonicalAliasAndTargetShareOneIdentity(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	alias := filepath.Join(tmp, "alias")

	require.NoError(t, os.MkdirAll(target, 0o755))
	require.NoError(t, os.Symlink(target, alias))

	assert.Equal(t, fsid.Canonical(target), fsid.Canonical(alias))
}

func TestCanonicalResolvesThroughTheDeepestExistingAncestor(t *testing.T) {
	t.Parallel()

	// Nothing exists beneath the alias or its target yet: the identity must
	// still match, resolved through the symlinked ancestor and rejoined with
	// the missing remainder, so a directory aliases before it is created.
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	alias := filepath.Join(tmp, "alias")

	require.NoError(t, os.MkdirAll(target, 0o755))
	require.NoError(t, os.Symlink(target, alias))

	assert.Equal(t,
		fsid.Canonical(filepath.Join(target, "child", ".ledger")),
		fsid.Canonical(filepath.Join(alias, "child", ".ledger")))
	assert.NotEqual(t,
		fsid.Canonical(filepath.Join(target, "one")),
		fsid.Canonical(filepath.Join(target, "two")),
		"distinct children keep distinct identities")
}

func TestWalkFilesVisitsEachPhysicalDirectoryOnce(t *testing.T) {
	t.Parallel()

	// One tree exercising every link shape at once: a sibling rename alias, a
	// subtree relocated behind a link, a link to a regular file, a dangling
	// link, and a cycle back to the root.
	tmp := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "a", "deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "a", "deep", "f1"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(tmp, "a"), filepath.Join(tmp, "b")))

	relocated := filepath.Join(t.TempDir(), "moved")
	require.NoError(t, os.MkdirAll(relocated, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(relocated, "f2"), []byte("y"), 0o600))
	require.NoError(t, os.Symlink(relocated, filepath.Join(tmp, "c")))

	require.NoError(t, os.WriteFile(filepath.Join(tmp, "plain"), []byte("z"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(tmp, "plain"), filepath.Join(tmp, "plain-link")))
	require.NoError(t, os.Symlink(filepath.Join(tmp, "gone"), filepath.Join(tmp, "dangling")))
	require.NoError(t, os.Symlink(tmp, filepath.Join(tmp, "a", "cycle")))

	var got []string

	require.NoError(t, fsid.WalkFiles(t.Context(), tmp, func(logical string) error {
		rel, err := filepath.Rel(tmp, logical)
		require.NoError(t, err)

		got = append(got, filepath.ToSlash(rel))

		return nil
	}))

	slices.Sort(got)

	// The sibling alias b contributes nothing (a's physical directory already
	// walked), the relocated subtree reports under its link, the file link
	// reports like its file, and the dangling link and cycle vanish quietly.
	assert.Equal(t, []string{"a/deep/f1", "c/f2", "plain", "plain-link"}, got)
}

func TestWalkFilesStopsOnCancel(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "f"), []byte("x"), 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := fsid.WalkFiles(ctx, tmp, func(string) error { return nil })
	require.ErrorIs(t, err, context.Canceled)
}
