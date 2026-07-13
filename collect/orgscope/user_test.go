package orgscope_test

import (
	"os"
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

func TestArchiveUser(t *testing.T) {
	t.Parallel()

	f := newOrgFixture(t)
	st := f.store

	user := &tfe.User{ID: "user-1", Username: "alice", Email: "alice@example.com"}
	require.NoError(t, f.collector.ArchiveUser(t.Context(), user))

	attrs := f.assertPrimary(t, st.User("user-1"), "users", "user-1")
	assert.Equal(t, "alice", attrs["username"])
	assert.Equal(t, "alice@example.com", attrs["email"])
}

func TestArchiveUserNilIsNoOp(t *testing.T) {
	t.Parallel()

	f := newOrgFixture(t)

	require.NoError(t, f.collector.ArchiveUser(t.Context(), nil))
}

func TestArchiveUserDeduplicates(t *testing.T) {
	t.Parallel()

	f := newOrgFixture(t)
	st := f.store

	user := &tfe.User{ID: "user-1", Username: "alice"}
	require.NoError(t, f.collector.ArchiveUser(t.Context(), user))

	// Remove the written file; a second archive of the same user is skipped by the
	// seen-set, so the file is not recreated.
	require.NoError(t, os.Remove(st.AbsPath(st.User("user-1"))))
	require.NoError(t, f.collector.ArchiveUser(t.Context(), user))

	exists, err := st.Exists(st.User("user-1"))
	require.NoError(t, err)
	assert.False(t, exists, "a repeated user is deduplicated and not re-archived")
}

func TestArchiveUserConcurrentDeduplicates(t *testing.T) {
	t.Parallel()

	f := newOrgFixture(t)
	st := f.store

	// Teams archive concurrently and often share members, so the same user
	// arrives from many goroutines at once; the seen-set claim must hand the
	// write to exactly one of them. The race detector guards the claim itself.
	user := &tfe.User{ID: "user-1", Username: "alice", Email: "alice@example.com"}

	var g errgroup.Group

	for range 16 {
		g.Go(func() error {
			return f.collector.ArchiveUser(t.Context(), user)
		})
	}

	require.NoError(t, g.Wait())

	attrs := f.assertPrimary(t, st.User("user-1"), "users", "user-1")
	assert.Equal(t, "alice", attrs["username"])
}
