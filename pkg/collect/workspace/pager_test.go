package workspace_test

import (
	"context"
	"slices"
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
	"go.jacobcolvin.com/hcp_archiver/pkg/collect/workspace"
)

// maxDrainPages bounds a drained walk, so a pager that never reports the end of
// its listing fails the test instead of hanging it.
const maxDrainPages = 20

// listing is an in-memory newest-first listing a pager pages over, with an edit
// hook applied before a chosen fetch answers, which is how a deletion or a
// creation is landed in the window between two of the walk's page fetches.
type listing struct {
	edit    func(fetch int, ids []string) []string
	ids     []string
	size    int
	fetches int
	noTotal bool
}

// page answers one page of the listing after applying any edit due before this
// fetch, reporting the pagination a page-number endpoint would.
func (l *listing) page(_ context.Context, page int) ([]string, *tfe.Pagination, error) {
	l.fetches++

	if l.edit != nil {
		l.ids = l.edit(l.fetches, l.ids)
	}

	start := min((page-1)*l.size, len(l.ids))
	end := min(start+l.size, len(l.ids))

	pg := &tfe.Pagination{CurrentPage: page, TotalCount: len(l.ids)}

	if end < len(l.ids) {
		pg.NextPage = page + 1
	}

	if l.noTotal {
		pg.TotalCount = 0
	}

	return slices.Clone(l.ids[start:end]), pg, nil
}

// deleteBefore returns an edit that drops ids from the listing just before the
// given fetch answers, the upstream deletion that shifts a live listing under a
// walk that is between pages.
func deleteBefore(fetch int, ids ...string) func(int, []string) []string {
	return func(n int, current []string) []string {
		if n != fetch {
			return current
		}

		return slices.DeleteFunc(current, func(id string) bool {
			return slices.Contains(ids, id)
		})
	}
}

// createBefore returns an edit that prepends id to the listing just before the
// given fetch answers, where a newest-first listing puts a newly created
// element.
func createBefore(fetch int, id string) func(int, []string) []string {
	return func(n int, current []string) []string {
		if n != fetch {
			return current
		}

		return append([]string{id}, current...)
	}
}

// drain walks pager the way [collect.Walk] does, from the first page to the end
// of the listing, and reports every element it was served in order.
func drain(t *testing.T, pager collect.Pager[string]) []string {
	t.Helper()

	served := []string{}

	for page := 1; page <= maxDrainPages; page++ {
		items, hasNext, err := pager(t.Context(), page)
		require.NoError(t, err)

		served = append(served, items...)

		if len(items) == 0 || !hasNext {
			return served
		}
	}

	t.Fatal("the pager never reported the end of the listing")

	return nil
}

func TestStablePagerServesEveryElementOnce(t *testing.T) {
	t.Parallel()

	// Six elements over pages of two, so the first page is archived (fetch 1) and
	// the listing is edited in the window before the second page is fetched
	// (fetch 2), the window in which a page-number pager loses an element.
	all := []string{"a1", "a2", "a3", "a4", "a5", "a6"}

	tests := map[string]struct {
		edit    func(int, []string) []string
		want    []string
		size    int
		noTotal bool
	}{
		"a steady listing serves every element once": {
			size: 2,
			want: all,
		},
		"a single page listing serves the whole listing": {
			size: 10,
			want: all,
		},
		// Without the re-list this is the silent gap: deleting a1 pulls a3 up into
		// the range page 1 already covered, so a3 is never listed, never archived,
		// and never recorded anywhere the ledger could notice it missing.
		"a deletion above the boundary still serves the element it shifted": {
			size: 2,
			edit: deleteBefore(2, "a1"),
			want: all,
		},
		"several deletions above the boundary still serve the elements they shifted": {
			size: 2,
			edit: deleteBefore(2, "a1", "a2"),
			want: all,
		},
		"a deletion below the boundary drops only the deleted element": {
			size: 2,
			edit: deleteBefore(2, "a6"),
			want: []string{"a1", "a2", "a3", "a4", "a5"},
		},
		// A creation lands at the newest end and pushes a served element down into
		// the next page; it is served once, not twice, and the new element is left
		// for the next walk, which starts above it.
		"a creation at the head serves no element twice": {
			size: 2,
			edit: createBefore(2, "a0"),
			want: all,
		},
		// An endpoint that omits the total leaves nothing to compare, so the walk
		// pages on as it always has.
		"a listing with no reported total still pages to the end": {
			size:    2,
			noTotal: true,
			want:    all,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			live := &listing{ids: slices.Clone(all), size: tc.size, edit: tc.edit, noTotal: tc.noTotal}

			got := drain(t, workspace.NewStablePager(func(id string) string { return id }, live.page))

			assert.Equal(t, tc.want, got, "every element that outlived the walk is served exactly once")
		})
	}
}

func TestStablePagerServesEveryElementAcrossRepeatedDeletionWaves(t *testing.T) {
	t.Parallel()

	// Two separate deletion waves during one walk. The second re-list must rewind
	// by the losses of both waves, since the served count still includes the
	// elements the first wave deleted: rewinding by only the latest loss starts
	// the scan past a7 and a8 and the walk ends without them.
	all := []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10"}

	firstWave := deleteBefore(2, "a1", "a2")
	secondWave := deleteBefore(5, "a3", "a4")

	live := &listing{
		ids:  slices.Clone(all),
		size: 2,
		edit: func(fetch int, current []string) []string {
			return secondWave(fetch, firstWave(fetch, current))
		},
	}

	got := drain(t, workspace.NewStablePager(func(id string) string { return id }, live.page))

	assert.Equal(t, all, got, "every element that outlived the walk is served exactly once")
}

func TestStablePagerSurvivesAShortMidListingPage(t *testing.T) {
	t.Parallel()

	// Fetch 2 answers a non-final page that runs short of the endpoint's page
	// window, the shape a listing edited mid-query produces. The span kept from
	// it must not shrink below the true window: a re-list divides the serve
	// boundary by the span, so a shrunken span restarts the scan past the
	// boundary and the elements the re-list exists to recover are skipped.
	all := []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10"}

	live := &listing{ids: slices.Clone(all), size: 2, edit: deleteBefore(3, "a1")}

	short := func(ctx context.Context, page int) ([]string, *tfe.Pagination, error) {
		items, pg, err := live.page(ctx, page)
		if live.fetches == 2 && len(items) == 2 {
			items = items[:1]
		}

		return items, pg, err
	}

	got := drain(t, workspace.NewStablePager(func(id string) string { return id }, short))

	assert.Equal(t, all, got, "the re-list recovers every element the short page and the deletion displaced")
}

func TestStablePagerStopsOnAListingThatKeepsShrinking(t *testing.T) {
	t.Parallel()

	ids := make([]string, 0, 20)
	for i := range 20 {
		ids = append(ids, string(rune('a'+i)))
	}

	// Every fetch from the second on loses an element, so no re-list ever reaches
	// a stable listing. The pager gives up rather than page over boundaries it
	// cannot trust; the walk aborts with it, leaving the collection unsettled so
	// the next run lists it again from the head.
	live := &listing{
		ids:  ids,
		size: 2,
		edit: func(fetch int, current []string) []string {
			if fetch < 2 || len(current) == 0 {
				return current
			}

			return current[:len(current)-1]
		},
	}

	pager := workspace.NewStablePager(func(id string) string { return id }, live.page)

	_, _, err := pager(t.Context(), 1)
	require.NoError(t, err, "the first page establishes the baseline")

	_, _, err = pager(t.Context(), 2)
	require.ErrorIs(t, err, workspace.ErrListingUnstable)
}
