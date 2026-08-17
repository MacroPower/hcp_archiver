package collect_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
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

	t.Run("concurrent fresh claims are race-free", func(t *testing.T) {
		t.Parallel()

		env, st, ledger := newEnv(t)

		// Distinct fresh claims across two parents run at once through the identity
		// mutex, exercising the shared owner cache (the outer map keyed by parent
		// and each inner map keyed by id) and the per-directory sidecar writes;
		// under -race this guards the mutex against a future narrowing.
		const perParent = 12

		parents := []string{"projects/a/workspaces", "projects/b/workspaces"}

		type claim struct{ dir, id string }

		var claims []claim

		for p, parent := range parents {
			for i := range perParent {
				claims = append(claims, claim{
					dir: fmt.Sprintf("%s/ws-%02d", parent, i),
					id:  fmt.Sprintf("id-%d-%02d", p, i),
				})
			}
		}

		errs := make([]error, len(claims))

		var wg sync.WaitGroup

		for i, c := range claims {
			wg.Go(func() {
				_, errs[i] = env.ClaimDir(c.dir, c.id)
			})
		}

		wg.Wait()

		for i, c := range claims {
			require.NoErrorf(t, errs[i], "claim %s", c.dir)
			assert.Equalf(t, c.id, readIdentity(t, st.Root(), c.dir).ID, "claim %s", c.dir)
		}

		assert.Zero(t, ledger.Tally().SurfacesDropped)
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

		// Renaming back clears the now-stale breadcrumb, stamps the directory
		// the object just abandoned as an orphan, and reports it like any other
		// rename.
		renamedFrom, err = env.ClaimDir(oldDir, "ws-1")
		require.NoError(t, err)
		assert.Equal(t, "app-renamed", renamedFrom)
		assert.Empty(t, readIdentity(t, st.Root(), oldDir).RenamedTo)

		abandoned := readIdentity(t, st.Root(), newDir)
		assert.Equal(t, "app", abandoned.RenamedTo,
			"the abandoned directory must not look like a live claim")
		assert.False(t, abandoned.RenamedAt.IsZero())
	})

	t.Run("a rename cycle across runs keeps the breadcrumb chain intact", func(t *testing.T) {
		t.Parallel()

		const (
			appDir  = "projects/p/workspaces/app"
			app2Dir = "projects/p/workspaces/app2"
			app3Dir = "projects/p/workspaces/app3"
		)

		root := t.TempDir()

		// Each run scans the siblings afresh: a fresh Env over the same root
		// models a new archiver invocation.
		claim := func(dir string) string {
			t.Helper()

			st := store.New(root)

			ledger, err := manifest.Load(st.Root(), manifest.WithClock(fixedClock()))
			require.NoError(t, err)

			defer func() { require.NoError(t, ledger.Close()) }()

			renamedFrom, err := collect.NewEnv(nil, st, ledger).ClaimDir(dir, "ws-1")
			require.NoError(t, err)

			return renamedFrom
		}

		assert.Empty(t, claim(appDir))
		assert.Equal(t, "app", claim(app2Dir))
		assert.Equal(t, "app2", claim(appDir))

		// The rename back must have stamped app2 as an orphan; otherwise this
		// final rename would resolve the prior directory to the stale orphan
		// (lexically last wins) and stamp the wrong sibling.
		assert.Equal(t, "app", claim(app3Dir))

		assert.Equal(t, "app3", readIdentity(t, root, appDir).RenamedTo)
		assert.Equal(t, "app", readIdentity(t, root, app2Dir).RenamedTo)
		assert.Empty(t, readIdentity(t, root, app3Dir).RenamedTo)
	})
}
