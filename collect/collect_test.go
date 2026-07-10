package collect_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/store"
)

// fixedClock returns a clock stuck at a fixed instant for deterministic ledger
// timestamps.
func fixedClock() func() time.Time {
	at := time.Date(2026, time.July, 8, 12, 0, 0, 0, time.UTC)

	return func() time.Time { return at }
}

// newEnv builds an [collect.Env] over a real store and ledger rooted in the
// test's temp dir, plus the ledger so a test can inspect and seed it.
func newEnv(t *testing.T) (*collect.Env, *store.Store, *manifest.Ledger) {
	t.Helper()

	root := t.TempDir()
	st := store.New(root)

	ledger, err := manifest.Load(st.Root(), manifest.WithClock(fixedClock()))
	require.NoError(t, err)

	env := collect.NewEnv(nil, st, ledger)

	return env, st, ledger
}

// cannedProject is a listed go-tfe object a fetch closure returns.
func cannedProject() *tfe.Project {
	return &tfe.Project{
		ID:                   "prj-abc",
		Name:                 "example",
		DefaultExecutionMode: "remote",
	}
}

// timeoutError is a net.Error whose timeout classifies as transient.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestEnvObject(t *testing.T) {
	t.Parallel()

	const relPath = "projects/example/project.json"

	tests := map[string]struct {
		fetch      func(called *bool) func(context.Context) (any, error)
		seed       func(l *manifest.Ledger)
		wantStatus manifest.Status
		wantFetch  bool
		wantSig    bool
	}{
		"records done and signature on success": {
			fetch: func(called *bool) func(context.Context) (any, error) {
				return func(_ context.Context) (any, error) {
					*called = true

					return cannedProject(), nil
				}
			},
			wantFetch:  true,
			wantStatus: manifest.StatusDone,
			wantSig:    true,
		},
		"records absent on a terminal error": {
			fetch: func(called *bool) func(context.Context) (any, error) {
				return func(_ context.Context) (any, error) {
					*called = true

					return nil, tfe.ErrResourceNotFound
				}
			},
			wantFetch:  true,
			wantStatus: manifest.StatusAbsentPermanently,
		},
		"records errored on a transient error": {
			fetch: func(called *bool) func(context.Context) (any, error) {
				return func(_ context.Context) (any, error) {
					*called = true

					return nil, timeoutError{}
				}
			},
			wantFetch:  true,
			wantStatus: manifest.StatusErrored,
		},
		"records forbidden on an access denial": {
			fetch: func(called *bool) func(context.Context) (any, error) {
				return func(_ context.Context) (any, error) {
					*called = true

					return nil, errors.New("forbidden\n\nTeam and Organization Tokens are not supported")
				}
			},
			wantFetch:  true,
			wantStatus: manifest.StatusForbidden,
		},
		"skips when the ledger has it settled": {
			seed: func(l *manifest.Ledger) {
				l.RecordDone(relPath, manifest.Signature{Hash: "seed", Size: 1})
			},
			fetch: func(called *bool) func(context.Context) (any, error) {
				return func(_ context.Context) (any, error) {
					*called = true

					return cannedProject(), nil
				}
			},
			wantFetch:  false,
			wantStatus: manifest.StatusDone,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			env, st, ledger := newEnv(t)

			if tc.seed != nil {
				tc.seed(ledger)
			}

			called := false

			err := env.Object(t.Context(), relPath, tc.fetch(&called))
			require.NoError(t, err)

			assert.Equal(t, tc.wantFetch, called)

			entry, ok := ledger.Entry(relPath)
			require.True(t, ok)
			assert.Equal(t, tc.wantStatus, entry.Status)

			if tc.wantSig {
				require.NotNil(t, entry.Signature)
				assert.NotEmpty(t, entry.Signature.Hash)
				assert.Positive(t, entry.Signature.Size)

				exists, existErr := st.Exists(relPath)
				require.NoError(t, existErr)
				assert.True(t, exists)

				assert.Equal(t, entry.Signature.Size, ledger.Tally().BytesDownloaded)
			}
		})
	}
}

func TestEnvObjectPropagatesCancellation(t *testing.T) {
	t.Parallel()

	env, _, ledger := newEnv(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := env.Object(ctx, "projects/example/project.json", func(ctx context.Context) (any, error) {
		return nil, ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)

	_, ok := ledger.Entry("projects/example/project.json")
	assert.False(t, ok, "a canceled fetch must not record an outcome")
}

func TestEnvMutableAlwaysRefetches(t *testing.T) {
	t.Parallel()

	const relPath = "projects/example/project.json"

	env, _, ledger := newEnv(t)
	ledger.RecordDone(relPath, manifest.Signature{Hash: "seed", Size: 1})

	called := false

	err := env.Mutable(t.Context(), relPath, func(_ context.Context) (any, error) {
		called = true

		return cannedProject(), nil
	})
	require.NoError(t, err)

	assert.True(t, called, "Mutable must re-fetch even a settled object")

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, entry.Status)
	assert.NotEqual(t, "seed", entry.Signature.Hash, "the refreshed signature replaces the seed")
}

func TestEnvBytes(t *testing.T) {
	t.Parallel()

	const relPath = "config-versions/cv-1.tar.gz"

	env, st, ledger := newEnv(t)

	err := env.Bytes(t.Context(), relPath, func(_ context.Context) ([]byte, error) {
		return []byte("tarball-bytes"), nil
	})
	require.NoError(t, err)

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, entry.Status)
	assert.Equal(t, int64(len("tarball-bytes")), entry.Signature.Size)

	exists, existErr := st.Exists(relPath)
	require.NoError(t, existErr)
	assert.True(t, exists)
}

func TestEnvBytesEmptyIsNotApplicable(t *testing.T) {
	t.Parallel()

	// A 204 No Content answer (an absent structured plan JSON, for one) reaches
	// the fetch as empty bytes with no error; it must be recorded as a settled
	// gap rather than written as a zero-byte, unparseable file.
	const relPath = "projects/example/workspaces/ws/runs/run-1/plan.json"

	env, st, ledger := newEnv(t)

	called := false
	err := env.Bytes(t.Context(), relPath, func(_ context.Context) ([]byte, error) {
		called = true

		return []byte{}, nil
	})
	require.NoError(t, err)
	assert.True(t, called)

	entry, ok := ledger.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusNotApplicable, entry.Status)
	assert.Nil(t, entry.Signature, "an empty payload records no content signature")

	exists, existErr := st.Exists(relPath)
	require.NoError(t, existErr)
	assert.False(t, exists, "no file is written for an empty payload")

	assert.Equal(t, int64(0), ledger.Tally().BytesDownloaded)
}

// walkItem is a listed element the walk describes and archives.
type walkItem struct {
	createdAt time.Time
	relPath   string
	terminal  bool
}

func TestWalkHaltsAndRevisits(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// Newest-first: r3 was archived while non-terminal (done, now still
	// non-terminal), r2 was archived done and is terminal (the frozen boundary,
	// here also the just-terminal transition), r1 is older terminal history.
	r3 := walkItem{relPath: "runs/r3/run.json", createdAt: base.Add(3 * time.Hour), terminal: false}
	r2 := walkItem{relPath: "runs/r2/run.json", createdAt: base.Add(2 * time.Hour), terminal: true}
	r1 := walkItem{relPath: "runs/r1/run.json", createdAt: base.Add(1 * time.Hour), terminal: true}

	env, _, ledger := newEnv(t)

	// Seed prior-run state: every element was archived done in a previous run
	// that walked the collection to its end, so the early stop is permitted.
	for _, it := range []walkItem{r3, r2, r1} {
		ledger.RecordDone(it.relPath, manifest.Signature{Hash: "prior", Size: 1})
	}

	ledger.MarkCollectionComplete("runs")

	archived := map[string]int{}

	pagesRequested := 0
	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		pagesRequested = page

		if page == 1 {
			return []walkItem{r3, r2, r1}, true, nil
		}

		return nil, false, nil
	}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				return env.Mutable(ctx, it.relPath, func(_ context.Context) (any, error) {
					archived[it.relPath]++

					return cannedProject(), nil
				})
			},
		}
	}

	err := collect.Walk(t.Context(), env, "runs", pager, describe)
	require.NoError(t, err)

	// Element r3 is non-terminal, so it is re-visited; r2 is the frozen boundary,
	// so it is archived once more (the transition refresh) and then the walk
	// halts before reaching r1.
	assert.Equal(t, 1, archived[r3.relPath], "non-terminal element is re-visited")
	assert.Equal(t, 1, archived[r2.relPath], "the boundary element gets a final refresh")
	assert.NotContains(t, archived, r1.relPath, "history past the boundary is never touched")

	assert.Equal(t, 1, pagesRequested, "the walk halts before requesting a second page")

	assert.Equal(t, r3.createdAt, ledger.HighWaterMark("runs"), "the mark advances to the newest element seen")
}

func TestWalkResumesIncompleteCollection(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// An interrupted first run archived the newest elements (r3, r2) but never
	// reached the older tail (r1) and never completed the walk.
	r3 := walkItem{relPath: "runs/r3/run.json", createdAt: base.Add(3 * time.Hour), terminal: true}
	r2 := walkItem{relPath: "runs/r2/run.json", createdAt: base.Add(2 * time.Hour), terminal: true}
	r1 := walkItem{relPath: "runs/r1/run.json", createdAt: base.Add(1 * time.Hour), terminal: true}

	env, _, ledger := newEnv(t)

	// The prior run recorded the newest elements done but was interrupted before
	// completing the collection, so it is not marked complete.
	ledger.RecordDone(r3.relPath, manifest.Signature{Hash: "prior", Size: 1})
	ledger.RecordDone(r2.relPath, manifest.Signature{Hash: "prior", Size: 1})

	archived := map[string]int{}

	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{r3, r2, r1}, false, nil
		}

		return nil, false, nil
	}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				return env.Object(ctx, it.relPath, func(_ context.Context) (any, error) {
					archived[it.relPath]++

					return cannedProject(), nil
				})
			},
		}
	}

	err := collect.Walk(t.Context(), env, "runs", pager, describe)
	require.NoError(t, err)

	// The walk does not stop at the newest already-done boundary; it pages all
	// the way down and reaches the un-archived older tail.
	assert.Equal(t, 1, archived[r1.relPath], "the un-archived older tail is reached and archived")
	assert.True(t, ledger.IsCollectionComplete("runs"), "reaching the final page marks the collection complete")
}

func TestWalkPropagatesPageError(t *testing.T) {
	t.Parallel()

	env, _, _ := newEnv(t)

	wantErr := tfe.ErrResourceNotFound
	pager := func(_ context.Context, _ int) ([]walkItem, bool, error) {
		return nil, false, wantErr
	}

	err := collect.Walk(t.Context(), env, "runs", pager, func(walkItem) collect.Item {
		return collect.Item{}
	})
	require.ErrorIs(t, err, wantErr)
}
