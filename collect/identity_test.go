package collect_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/collect"
)

// readIdentity reads the identity sidecar under the archive-relative dir.
func readIdentity(t *testing.T, root, dir string) collect.Identity {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), ".identity.json"))
	require.NoError(t, err)

	var out collect.Identity

	require.NoError(t, json.Unmarshal(raw, &out))

	return out
}

func TestClaimDir(t *testing.T) {
	t.Parallel()

	const dir = "projects/p/workspaces/app"

	t.Run("a fresh directory is claimed", func(t *testing.T) {
		t.Parallel()

		env, st, ledger := newEnv(t)

		renamedFrom, err := env.ClaimDir(dir, "ws-1")
		require.NoError(t, err)
		assert.Empty(t, renamedFrom)

		assert.Equal(t, "ws-1", readIdentity(t, st.Root(), dir).ID)
		assert.Zero(t, ledger.Tally().SurfacesDropped)
	})

	t.Run("a re-claim by the same owner passes", func(t *testing.T) {
		t.Parallel()

		env, _, _ := newEnv(t)

		_, err := env.ClaimDir(dir, "ws-1")
		require.NoError(t, err)

		renamedFrom, err := env.ClaimDir(dir, "ws-1")
		require.NoError(t, err)
		assert.Empty(t, renamedFrom)
	})

	t.Run("a reused name fails closed", func(t *testing.T) {
		t.Parallel()

		env, st, ledger := newEnv(t)

		_, err := env.ClaimDir(dir, "ws-old")
		require.NoError(t, err)

		// The workspace named app was deleted and a new one took the name.
		// Archiving in place would overwrite the deleted workspace's final
		// metadata, so the claim must fail, leave the sidecar untouched, and
		// mark the run incomplete.
		_, err = env.ClaimDir(dir, "ws-new")
		require.ErrorIs(t, err, collect.ErrIdentityMismatch)

		assert.Equal(t, "ws-old", readIdentity(t, st.Root(), dir).ID,
			"the deleted workspace keeps its directory")
		assert.Equal(t, 1, ledger.Tally().SurfacesDropped)
	})

	t.Run("a corrupt sidecar fails closed", func(t *testing.T) {
		t.Parallel()

		env, st, ledger := newEnv(t)

		abs := filepath.Join(st.Root(), "projects", "p", "workspaces", "app")
		require.NoError(t, os.MkdirAll(abs, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(abs, ".identity.json"), []byte("{"), 0o600))

		_, err := env.ClaimDir(dir, "ws-1")
		require.Error(t, err, "unverifiable ownership must not be guessed at")
		assert.Equal(t, 1, ledger.Tally().SurfacesDropped)
	})

	t.Run("a rename leaves a breadcrumb in the old directory", func(t *testing.T) {
		t.Parallel()

		const (
			oldDir = "projects/p/workspaces/app"
			newDir = "projects/p/workspaces/app-renamed"
		)

		env, st, ledger := newEnv(t)

		_, err := env.ClaimDir(oldDir, "ws-1")
		require.NoError(t, err)

		// The workspace was renamed upstream: the fresh directory's claim spots
		// the sibling archiving the same id, reports it, and stamps the old
		// directory, which stays in place as kept history.
		renamedFrom, err := env.ClaimDir(newDir, "ws-1")
		require.NoError(t, err)
		assert.Equal(t, "app", renamedFrom)

		old := readIdentity(t, st.Root(), oldDir)
		assert.Equal(t, "ws-1", old.ID)
		assert.Equal(t, "app-renamed", old.RenamedTo)
		assert.False(t, old.RenamedAt.IsZero())

		assert.Equal(t, "ws-1", readIdentity(t, st.Root(), newDir).ID)
		assert.Zero(t, ledger.Tally().SurfacesDropped, "a rename is not a failure")

		// Renaming back clears the now-stale breadcrumb.
		_, err = env.ClaimDir(oldDir, "ws-1")
		require.NoError(t, err)
		assert.Empty(t, readIdentity(t, st.Root(), oldDir).RenamedTo)
	})
}
