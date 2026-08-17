package collect_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
)

// errArchive is the failure a shared archive returns when a test needs the
// claim to see an unsettled outcome.
var errArchive = errors.New("archive failed")

func TestArchiveShared(t *testing.T) {
	t.Parallel()

	const relPath = "users/user-1.json"

	t.Run("concurrent callers archive once and all see the outcome", func(t *testing.T) {
		t.Parallel()

		env, _, _ := newEnv(t)

		var calls atomic.Int64

		errs := make([]error, 16)

		var wg sync.WaitGroup

		for i := range errs {
			wg.Go(func() {
				errs[i] = env.ArchiveShared(t.Context(), relPath, func(ctx context.Context) error {
					calls.Add(1)

					return env.Mutable(ctx, relPath, func(context.Context) (any, error) {
						return cannedProject(), nil
					})
				})
			})
		}

		wg.Wait()

		assert.Equal(t, int64(1), calls.Load(), "one caller archives, the rest wait on it")

		for i, err := range errs {
			require.NoErrorf(t, err, "caller %d", i)
		}
	})

	t.Run("the object is settled by the time every caller returns", func(t *testing.T) {
		t.Parallel()

		env, _, _ := newEnv(t)

		// The gate a caller mirrors its write into ([collect.Env.Reference]) reads
		// exactly this predicate, and reads it the moment ArchiveShared returns. A
		// claim that let a non-claimant run ahead of the write would open that gate
		// over an object that was about to settle, stranding the referencing walk.
		var g errgroup.Group

		settled := make([]bool, 16)

		for i := range settled {
			g.Go(func() error {
				err := env.ArchiveShared(t.Context(), relPath, func(ctx context.Context) error {
					return env.Mutable(ctx, relPath, func(context.Context) (any, error) {
						return cannedProject(), nil
					})
				})
				if err != nil {
					return fmt.Errorf("archive shared: %w", err)
				}

				settled[i] = !env.ShouldFetch(relPath)

				return nil
			})
		}

		require.NoError(t, g.Wait())

		for i, ok := range settled {
			assert.Truef(t, ok, "caller %d returned before the object settled", i)
		}
	})

	t.Run("a repeat over a settled object does not re-archive", func(t *testing.T) {
		t.Parallel()

		env, _, _ := newEnv(t)

		var calls int

		archive := func(ctx context.Context) error {
			calls++

			return env.Mutable(ctx, relPath, func(context.Context) (any, error) {
				return cannedProject(), nil
			})
		}

		require.NoError(t, env.ArchiveShared(t.Context(), relPath, archive))
		require.NoError(t, env.ArchiveShared(t.Context(), relPath, archive))

		assert.Equal(t, 1, calls, "the claim holds while the object stays settled")
	})

	t.Run("a failed archive releases the claim for the run's own retry", func(t *testing.T) {
		t.Parallel()

		env, _, ledger := newEnv(t)

		// The next caller to name the path is the only retry the run has: the
		// reference gate keeps the failure visible to a later run, but nothing
		// re-drives the current one, so a claim outliving the failure would hand
		// every remaining reference the same error.
		require.ErrorIs(t, env.ArchiveShared(t.Context(), relPath, func(context.Context) error {
			return errArchive
		}), errArchive)

		var retried bool

		require.NoError(t, env.ArchiveShared(t.Context(), relPath, func(ctx context.Context) error {
			retried = true

			return env.Mutable(ctx, relPath, func(context.Context) (any, error) {
				return cannedProject(), nil
			})
		}))

		assert.True(t, retried, "the next caller re-archives an object left unsettled")

		entry, ok := ledger.Entry(relPath)
		require.True(t, ok)
		assert.Equal(t, manifest.StatusDone, entry.Status)
	})

	t.Run("an error over a settled object is not memoized", func(t *testing.T) {
		t.Parallel()

		env, _, _ := newEnv(t)

		// Settle the object outside any claim, as a prior run's loaded ledger
		// would have: the object is Done but no claim stands for it yet.
		require.NoError(t, env.Mutable(t.Context(), relPath, func(context.Context) (any, error) {
			return cannedProject(), nil
		}))

		// A claimant erring without unsettling anything (a cancellation from
		// its own dying context is the real shape) must not leave its error
		// standing for every later caller of the path.
		require.ErrorIs(t, env.ArchiveShared(t.Context(), relPath, func(context.Context) error {
			return errArchive
		}), errArchive)

		require.NoError(t, env.ArchiveShared(t.Context(), relPath, func(context.Context) error {
			// The object is still settled, so the fresh claim archives freely
			// and the repeat costs one call, not an inherited failure.
			return nil
		}), "a later caller with a live context recovers")
	})

	t.Run("distinct paths archive independently", func(t *testing.T) {
		t.Parallel()

		env, _, _ := newEnv(t)

		var calls atomic.Int64

		paths := []string{"users/user-1.json", "users/user-2.json", "users/user-3.json"}

		var g errgroup.Group

		for _, p := range paths {
			g.Go(func() error {
				return env.ArchiveShared(t.Context(), p, func(ctx context.Context) error {
					calls.Add(1)

					return env.Mutable(ctx, p, func(context.Context) (any, error) {
						return cannedProject(), nil
					})
				})
			})
		}

		require.NoError(t, g.Wait())

		assert.Equal(t, int64(len(paths)), calls.Load(), "one claim per path, not one overall")
	})

	t.Run("a waiter stops waiting when its own context ends", func(t *testing.T) {
		t.Parallel()

		env, _, _ := newEnv(t)

		release := make(chan struct{})
		claimed := make(chan struct{})

		var g errgroup.Group

		g.Go(func() error {
			return env.ArchiveShared(t.Context(), relPath, func(context.Context) error {
				close(claimed)
				<-release

				return nil
			})
		})

		<-claimed

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err := env.ArchiveShared(ctx, relPath, func(context.Context) error {
			assert.Fail(t, "a waiter must not archive")

			return nil
		})
		require.ErrorIs(t, err, context.Canceled)

		close(release)
		require.NoError(t, g.Wait())
	})
}

// TestArchiveSharedKeepsOneVersionInTheSidecar pins the damage end to end:
// concurrent archives of one object superseding the same outgoing bytes into
// its history sidecar, each recording the version the others already did.
//
// Either mechanism alone holds this line, the claim by collapsing the writers
// and the store by serializing whatever survives, so this guards the composed
// behavior rather than one of them. The subtests above are what pin the claim
// itself.
func TestArchiveSharedKeepsOneVersionInTheSidecar(t *testing.T) {
	t.Parallel()

	const relPath = "users/user-1.json"

	env, st, _ := newEnv(t)

	// A prior run's copy, so the concurrent archives below all find content to
	// supersede rather than an absent file.
	require.NoError(t, env.Mutable(t.Context(), relPath, func(context.Context) (any, error) {
		return cannedProject(), nil
	}))

	var g errgroup.Group

	for range 16 {
		g.Go(func() error {
			return env.ArchiveShared(t.Context(), relPath, func(ctx context.Context) error {
				return env.Mutable(ctx, relPath, func(context.Context) (any, error) {
					return cannedProjectRenamed(), nil
				})
			})
		})
	}

	require.NoError(t, g.Wait())

	records := sidecarRecords(t, st, relPath)
	require.Len(t, records, 1, "the superseded version is recorded once, not once per racing writer")
	assert.NotEmpty(t, records[0].Content)
}

// cannedProjectRenamed is [cannedProject] under a different name, so a second
// archive of one path is a changed payload that supersedes the first.
func cannedProjectRenamed() *tfe.Project {
	p := cannedProject()
	p.Name = "renamed"

	return p
}
