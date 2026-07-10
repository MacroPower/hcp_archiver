package collect_test

import (
	"context"
	"errors"
	"fmt"
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

func TestWalkEarlyStopsWhenFullySettled(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// A fully settled collection: every element terminal and archived done, the
	// collection walked to its end and recorded settled, with no unsettled child.
	// The walk must still stop at the newest boundary so the incremental
	// optimization is preserved.
	r3 := walkItem{relPath: "runs/r3/run.json", createdAt: base.Add(3 * time.Hour), terminal: true}
	r2 := walkItem{relPath: "runs/r2/run.json", createdAt: base.Add(2 * time.Hour), terminal: true}
	r1 := walkItem{relPath: "runs/r1/run.json", createdAt: base.Add(1 * time.Hour), terminal: true}

	env, _, ledger := newEnv(t)

	for _, it := range []walkItem{r3, r2, r1} {
		ledger.RecordDone(it.relPath, manifest.Signature{Hash: "prior", Size: 1})
	}

	ledger.MarkCollectionComplete("runs")
	ledger.SetCollectionSettled("runs", true)

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

	// Only r3, the newest boundary, is refreshed; the walk halts before touching
	// the settled history below it or requesting a second page.
	assert.Equal(t, 1, archived[r3.relPath], "the newest boundary gets a final refresh")
	assert.NotContains(t, archived, r2.relPath, "settled history is not re-touched")
	assert.NotContains(t, archived, r1.relPath, "settled history is not re-touched")

	assert.Equal(t, 1, pagesRequested, "the walk halts before requesting a second page")

	assert.Equal(t, r3.createdAt, ledger.HighWaterMark("runs"), "the mark advances to the newest element seen")
}

func TestWalkPagesPastNonTerminalBoundary(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// A newer run finished terminal while an older run is still in flight
	// (recorded done but non-terminal, as a run archives its summary done and
	// defers its children). Old behavior stopped at the newer terminal boundary
	// and stranded the older run's pending children; the fix must page past it.
	newer := walkItem{relPath: "runs/rNew/run.json", createdAt: base.Add(3 * time.Hour), terminal: true}
	older := walkItem{relPath: "runs/rOld/run.json", createdAt: base.Add(1 * time.Hour), terminal: false}

	env, _, ledger := newEnv(t)

	ledger.RecordDone(newer.relPath, manifest.Signature{Hash: "prior", Size: 1})
	ledger.RecordDone(older.relPath, manifest.Signature{Hash: "prior", Size: 1})

	// The collection completed a prior walk, so the old early-stop would fire at
	// the newer boundary; settled is unset, so the fix keeps paging.
	ledger.MarkCollectionComplete("runs")

	archived := map[string]int{}

	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{newer, older}, false, nil
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

	// The walk pages past the newer terminal boundary and revisits the older
	// in-flight run, which old behavior would have stranded.
	assert.Equal(t, 1, archived[newer.relPath], "the newer terminal boundary is refreshed")
	assert.Equal(t, 1, archived[older.relPath], "the older in-flight run is reached and refreshed")

	// Seeing a non-terminal element leaves the collection unsettled, so the next
	// run re-pages until it finishes.
	assert.False(t, ledger.IsCollectionSettled("runs"), "an in-flight run keeps the collection unsettled")
	assert.True(t, ledger.IsCollectionComplete("runs"), "reaching the final page still marks completion")
}

func TestWalkReachesErroredChildBelowBoundary(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// Both runs are terminal and their summaries archived done, and the
	// collection is marked complete and settled — yet an older run's child log
	// errored in a prior run and was never retried. The start gate's
	// HasUnsettledUnder check must force a full re-walk so the errored child is
	// reached.
	r2 := walkItem{relPath: "runs/r2/run.json", createdAt: base.Add(2 * time.Hour), terminal: true}
	r1 := walkItem{relPath: "runs/r1/run.json", createdAt: base.Add(1 * time.Hour), terminal: true}

	env, _, ledger := newEnv(t)

	ledger.RecordDone(r2.relPath, manifest.Signature{Hash: "prior", Size: 1})
	ledger.RecordDone(r1.relPath, manifest.Signature{Hash: "prior", Size: 1})

	const erroredChild = "runs/r1/plan.log"

	ledger.RecordErrored(erroredChild, errors.New("prior boom"), true)

	ledger.MarkCollectionComplete("runs")
	// The steady-state hole: a prior all-terminal walk recorded settled even
	// though a child stayed errored. The start gate closes it.
	ledger.SetCollectionSettled("runs", true)

	archivedChild := 0

	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{r2, r1}, false, nil
		}

		return nil, false, nil
	}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				err := env.Mutable(ctx, it.relPath, func(_ context.Context) (any, error) {
					return cannedProject(), nil
				})
				if err != nil {
					return fmt.Errorf("archive run summary: %w", err)
				}

				if it.relPath != r1.relPath {
					return nil
				}

				// The older run retries its errored child once the walk reaches it.
				return env.Object(ctx, erroredChild, func(_ context.Context) (any, error) {
					archivedChild++

					return cannedProject(), nil
				})
			},
		}
	}

	err := collect.Walk(t.Context(), env, "runs", pager, describe)
	require.NoError(t, err)

	assert.Equal(t, 1, archivedChild, "the errored child below the done boundary is reached and retried")

	child, ok := ledger.Entry(erroredChild)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, child.Status, "the retried child is now settled done")

	// With the child fixed, the completing walk records the collection settled
	// again, so a later run may early-stop.
	assert.True(t, ledger.IsCollectionSettled("runs"), "the collection settles once no unsettled child remains")
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

// stackConfigCursorKey is a synthetic stack-configuration cursor: it routes to
// the org-root shard, while the configuration entries live in a stack shard.
const stackConfigCursorKey = "stacks/stk-1/configurations"

// erroredChildScenario is a Walk mimicking a stack's configuration walk, whose
// cursor key routes to a different shard than its entries, with an older
// configuration's child left errored below a newer done boundary.
type erroredChildScenario struct {
	env           *collect.Env
	ledger        *manifest.Ledger
	pager         collect.Pager[walkItem]
	describe      func(walkItem) collect.Item
	erroredChild  string
	archivedChild *int
}

// newErroredChildScenario seeds two terminal configurations archived done in a
// stack shard, marks the collection complete and settled under its synthetic
// cursor, and leaves the older configuration's child errored. The older
// configuration's Archive closure retries that child and bumps a counter.
func newErroredChildScenario(t *testing.T) *erroredChildScenario {
	t.Helper()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	cfg2 := walkItem{
		relPath:   "projects/p/stacks/s/configurations/cfg2/configuration.json",
		createdAt: base.Add(2 * time.Hour),
		terminal:  true,
	}
	cfg1 := walkItem{
		relPath:   "projects/p/stacks/s/configurations/cfg1/configuration.json",
		createdAt: base.Add(1 * time.Hour),
		terminal:  true,
	}

	env, _, ledger := newEnv(t)

	ledger.RecordDone(cfg2.relPath, manifest.Signature{Hash: "prior", Size: 1})
	ledger.RecordDone(cfg1.relPath, manifest.Signature{Hash: "prior", Size: 1})

	const erroredChild = "projects/p/stacks/s/configurations/cfg1/json-schemas.json"

	ledger.RecordErrored(erroredChild, errors.New("prior boom"), true)

	ledger.MarkCollectionComplete(stackConfigCursorKey)
	ledger.SetCollectionSettled(stackConfigCursorKey, true)

	archivedChild := 0

	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{cfg2, cfg1}, false, nil
		}

		return nil, false, nil
	}

	describe := func(it walkItem) collect.Item {
		return collect.Item{
			RelPath:   it.relPath,
			CreatedAt: it.createdAt,
			Terminal:  it.terminal,
			Archive: func(ctx context.Context) error {
				err := env.Mutable(ctx, it.relPath, func(_ context.Context) (any, error) {
					return cannedProject(), nil
				})
				if err != nil {
					return fmt.Errorf("archive configuration: %w", err)
				}

				if it.relPath != cfg1.relPath {
					return nil
				}

				return env.Object(ctx, erroredChild, func(_ context.Context) (any, error) {
					archivedChild++

					return cannedProject(), nil
				})
			},
		}
	}

	return &erroredChildScenario{
		env:           env,
		ledger:        ledger,
		pager:         pager,
		describe:      describe,
		erroredChild:  erroredChild,
		archivedChild: &archivedChild,
	}
}

func TestWalkArchivePrefixReachesErroredChildInAnotherShard(t *testing.T) {
	t.Parallel()

	s := newErroredChildScenario(t)

	// The archive prefix points the errored-child gate at the stack shard that
	// actually holds the entries, so the walk re-pages past the done boundary and
	// retries the older configuration's errored child.
	err := collect.Walk(t.Context(), s.env, stackConfigCursorKey, s.pager, s.describe,
		collect.WithArchivePrefix("projects/p/stacks/s/configurations"))
	require.NoError(t, err)

	assert.Equal(t, 1, *s.archivedChild, "the archive prefix reaches the errored child in the entries' shard")

	child, ok := s.ledger.Entry(s.erroredChild)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusDone, child.Status, "the retried child settles done")
	assert.True(t, s.ledger.IsCollectionSettled(stackConfigCursorKey),
		"the collection settles once no unsettled child remains")
}

func TestWalkSyntheticCursorStrandsErroredChildWithoutArchivePrefix(t *testing.T) {
	t.Parallel()

	s := newErroredChildScenario(t)

	// Without the override the gate scans the org-root shard the synthetic cursor
	// routes to, not the stack shard holding the entries, so the done boundary
	// early-stops and the older errored child stays stranded. This is the default
	// the override exists to close.
	err := collect.Walk(t.Context(), s.env, stackConfigCursorKey, s.pager, s.describe)
	require.NoError(t, err)

	assert.Zero(t, *s.archivedChild, "without an archive prefix the errored child is not reached")
}

func TestWalkFinalPageRecomputesSettledFromArchivePrefix(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC)

	// A first full walk reaches the final page, where it recomputes the settled
	// flag. That recompute must consult the archive prefix, not the synthetic
	// cursor: an errored child under the prefix (in the stack shard) must record
	// the collection unsettled so the next run re-walks it. A recompute keyed on
	// the cursor would scan the empty org-root shard and wrongly record settled.
	const (
		cursorKey     = "stacks/stk-2/configurations"
		archivePrefix = "projects/p/stacks/s2/configurations"
		erroredChild  = archivePrefix + "/cfg1/json-schemas.json"
	)

	cfg2 := walkItem{
		relPath:   archivePrefix + "/cfg2/configuration.json",
		createdAt: base.Add(2 * time.Hour),
		terminal:  true,
	}
	cfg1 := walkItem{
		relPath:   archivePrefix + "/cfg1/configuration.json",
		createdAt: base.Add(1 * time.Hour),
		terminal:  true,
	}

	env, _, ledger := newEnv(t)

	// An errored child left under the prefix that the walk does not retry.
	ledger.RecordErrored(erroredChild, errors.New("prior boom"), true)

	pager := func(_ context.Context, page int) ([]walkItem, bool, error) {
		if page == 1 {
			return []walkItem{cfg2, cfg1}, false, nil
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
					return cannedProject(), nil
				})
			},
		}
	}

	err := collect.Walk(t.Context(), env, cursorKey, pager, describe,
		collect.WithArchivePrefix(archivePrefix))
	require.NoError(t, err)

	assert.True(t, ledger.IsCollectionComplete(cursorKey), "the final page marks the collection complete")
	assert.False(t, ledger.IsCollectionSettled(cursorKey),
		"an errored child under the archive prefix records the collection unsettled")
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
