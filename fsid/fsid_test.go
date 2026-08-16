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

	aliases, err := fsid.WalkFiles(t.Context(), tmp, func(logical string) error {
		rel, relErr := filepath.Rel(tmp, logical)
		require.NoError(t, relErr)

		got = append(got, filepath.ToSlash(rel))

		return nil
	})
	require.NoError(t, err)

	slices.Sort(got)

	// The sibling alias b contributes no files (a's physical directory owns
	// them), the relocated subtree reports under its link, the file link
	// reports like its file, and the dangling link vanishes quietly.
	assert.Equal(t, []string{"a/deep/f1", "c/f2", "plain", "plain-link"}, got)

	// The declined intra-tree links surface as aliases: the sibling rename
	// link maps to its target, and the cycle maps to the root that owns it.
	assert.Equal(t, map[string]string{
		filepath.Join(tmp, "b"):          filepath.Join(tmp, "a"),
		filepath.Join(tmp, "a", "cycle"): tmp,
	}, aliases)
}

func TestWalkFilesNamesArePhysicalRegardlessOfVisitOrder(t *testing.T) {
	t.Parallel()

	// A rename alias that sorts lexically BEFORE its target must not claim
	// the subtree's files: every other name in the system (ledger entries,
	// remote keys) derives from the physical layout, so a walk that reported
	// the link's name would disagree with all of them: the keep set would
	// miss every real key and the prune would delete the live mirror.
	tmp := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "target"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "target", "f"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(tmp, "target"), filepath.Join(tmp, "0-alias")))

	var got []string

	aliases, err := fsid.WalkFiles(t.Context(), tmp, func(logical string) error {
		rel, relErr := filepath.Rel(tmp, logical)
		require.NoError(t, relErr)

		got = append(got, filepath.ToSlash(rel))

		return nil
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"target/f"}, got, "the physical name wins whichever sorts first")
	assert.Equal(t, map[string]string{
		filepath.Join(tmp, "0-alias"): filepath.Join(tmp, "target"),
	}, aliases)
}

func TestWalkFilesStopsOnCancel(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "f"), []byte("x"), 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := fsid.WalkFiles(ctx, tmp, func(string) error { return nil })
	require.ErrorIs(t, err, context.Canceled)
}

func TestWalkFilesDeclinesAnOwnedTreeInsideAFollowedTarget(t *testing.T) {
	t.Parallel()

	// Two relocation links whose targets nest: whichever order the walk
	// visits them, the shared subtree must report exactly once and the
	// duplicate prefix must surface as an alias, or its files would report
	// twice under two logical names depending on lexical order.
	for name, links := range map[string]struct{ inner, parent string }{
		"inner link sorts first":  {inner: "a-inner", parent: "b-parent"},
		"parent link sorts first": {inner: "b-inner", parent: "a-parent"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ext := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(ext, "data", "x"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(ext, "data", "x", "f"), []byte("x"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(ext, "data", "g"), []byte("g"), 0o600))

			tmp := t.TempDir()
			require.NoError(t, os.Symlink(filepath.Join(ext, "data", "x"), filepath.Join(tmp, links.inner)))
			require.NoError(t, os.Symlink(filepath.Join(ext, "data"), filepath.Join(tmp, links.parent)))

			var got []string

			aliases, err := fsid.WalkFiles(t.Context(), tmp, func(logical string) error {
				rel, relErr := filepath.Rel(tmp, logical)
				require.NoError(t, relErr)

				got = append(got, filepath.ToSlash(rel))

				return nil
			})
			require.NoError(t, err)

			slices.Sort(got)

			fs := 0
			for _, p := range got {
				if filepath.Base(p) == "f" {
					fs++
				}
			}

			assert.Equal(t, 1, fs, "the nested subtree's file reports exactly once: %v", got)
			assert.Len(t, got, 2, "one report per physical file: %v", got)
			assert.NotEmpty(t, aliases, "the declined duplicate prefix surfaces as an alias")
		})
	}
}

func TestWalkFilesDeclinesALinkToAnAncestorOfTheRoot(t *testing.T) {
	t.Parallel()

	// A link pointing above the walk root contains the whole walked tree:
	// following it would re-report the entire archive under the link's name.
	tmp := t.TempDir()
	root := filepath.Join(tmp, "archive")

	require.NoError(t, os.MkdirAll(filepath.Join(root, "ws"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "ws", "f"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(tmp, filepath.Join(root, "up")))

	var got []string

	aliases, err := fsid.WalkFiles(t.Context(), root, func(logical string) error {
		rel, relErr := filepath.Rel(root, logical)
		require.NoError(t, relErr)

		got = append(got, filepath.ToSlash(rel))

		return nil
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"ws/f"}, got, "the archive reports once, not again beneath the link")
	assert.Equal(t, map[string]string{
		filepath.Join(root, "up", "archive"): root,
	}, aliases)
}

//nolint:paralleltest // t.Chdir is incompatible with t.Parallel, and the case needs a working-directory-relative root.
func TestWalkFilesRelativeRootWithAbsoluteLinkTarget(t *testing.T) {
	// A relative walk root meets a symlink whose resolution is absolute
	// (filepath.EvalSymlinks answers absolute whenever any link component
	// is). The ownership comparisons must still match, or the same physical
	// subtree walks and reports twice with no alias recorded.
	base := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(base, "demo", "tree", "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "demo", "tree", "sub", "inner.txt"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(base, "demo", "tree", "sub"), filepath.Join(base, "demo", "backlink")))

	t.Chdir(base)

	var got []string

	aliases, err := fsid.WalkFiles(t.Context(), "demo", func(logical string) error {
		got = append(got, filepath.ToSlash(logical))

		return nil
	})
	require.NoError(t, err)

	assert.Len(t, got, 1, "one physical file reports exactly once: %v", got)
	assert.Len(t, aliases, 1, "the declined second spelling surfaces as an alias: %v", aliases)
}
