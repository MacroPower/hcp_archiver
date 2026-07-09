package collect

import (
	"context"
	"fmt"
	"time"

	"github.com/MacroPower/tfc_archiver/manifest"
)

// Item describes one element of an append-mostly collection to [Walk].
//
// A collector produces it from a listed object: RelPath is the ledger key and
// primary file of the element, CreatedAt orders the collection and drives its
// high-water mark, Terminal reports whether the element has frozen (a finished
// run versus a running one), and Archive commits the element and any children
// through the [Env] primitives.
type Item struct {
	// Archive archives the element (its own object and any children) through the
	// [Env] primitives. It returns only on a context cancellation, matching the
	// primitives' best-effort contract.
	Archive func(context.Context) error
	// CreatedAt is the element's creation time; the newest seen advances the
	// collection's high-water mark.
	CreatedAt time.Time
	// RelPath is the element's primary file and its ledger key, the same string
	// its Archive closure passes to the [Env] primitive that writes that file.
	RelPath string
	// Terminal reports whether the element has settled into a terminal state and
	// so needs no further refresh once archived.
	Terminal bool
}

// Pager fetches one page of a newest-first paginated collection.
//
// Page numbers start at 1 for the newest page. It reports the page's items in
// newest-first order and whether a further page exists, so [Walk] can stop
// paging the moment it reaches already-archived history.
type Pager[T any] func(ctx context.Context, page int) (items []T, hasNext bool, err error)

// Walk archives an append-mostly collection newest-first and halts as soon as it
// reaches already-archived history, the incremental re-run engine shared by every
// ordered collection (state versions, runs, per-run children, config versions).
//
// It pages through page newest-first, and for each element: advances the
// collection's high-water mark under key toward the element's CreatedAt, archives
// the element, and, once it archives an element that was already recorded done
// and is now terminal, stops. It archives that boundary element before stopping
// so a run that has just transitioned from running to finished still gets the
// final refresh of its mutable tail; a still-running element is re-archived and
// the walk continues, while brand-new elements append. Immutable children the
// element's Archive closure writes through [Env.Object], [Env.Blob], or
// [Env.Bytes] are skipped on that boundary refresh because the ledger already has
// them settled.
//
// Only a context cancellation surfaced by an Archive closure or a page fetch
// aborts the walk; every per-object error is recorded by the primitives and the
// walk continues.
func Walk[T any](
	ctx context.Context,
	env *Env,
	key string,
	page Pager[T],
	describe func(T) Item,
) error {
	for pageNum := 1; ; pageNum++ {
		items, hasNext, err := page(ctx, pageNum)
		if err != nil {
			return fmt.Errorf("list %q page %d: %w", key, pageNum, err)
		}

		for _, listed := range items {
			item := describe(listed)

			env.ledger.AdvanceHighWaterMark(key, item.CreatedAt)

			entry, ok := env.ledger.Entry(item.RelPath)
			frozen := ok && entry.Status == manifest.StatusDone && item.Terminal

			archiveErr := item.Archive(ctx)
			if archiveErr != nil {
				return archiveErr
			}

			if frozen {
				return nil
			}
		}

		if !hasNext {
			return nil
		}
	}
}
