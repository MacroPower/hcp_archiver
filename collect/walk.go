package collect

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"go.jacobcolvin.com/hcp_archiver/manifest"
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

// walkConfig holds the resolved settings for one [Walk].
type walkConfig struct {
	historyOldest time.Time
	historyCount  int
}

// bounded reports whether a history limit is configured.
func (c *walkConfig) bounded() bool {
	return c.historyCount > 0 || !c.historyOldest.IsZero()
}

// withinBounds reports whether the element at listing position idx (zero-based,
// newest first), created at createdAt, falls inside at least one configured
// history bound. The count and age bounds are each monotone over a newest-first
// listing, so the first element outside both is the start of an excluded tail.
func (c *walkConfig) withinBounds(idx int, createdAt time.Time) bool {
	if c.historyCount > 0 && idx < c.historyCount {
		return true
	}

	if !c.historyOldest.IsZero() && !createdAt.Before(c.historyOldest) {
		return true
	}

	return false
}

// WalkOption configures a [Walk].
//
// Options of this type:
//   - [WithHistoryLimit]
type WalkOption func(*walkConfig)

// WithHistoryLimit bounds the walk to the collection's recent history: an
// element is within the limit while it sits among the newest count listed
// elements (when count is positive) or was created at or after oldest (when
// oldest is non-zero), so configuring both keeps whichever window admits more
// history. Both bounds are monotone over the newest-first listing, so the
// first element outside every configured bound starts an excluded tail: the
// walk stops there without archiving it or anything older.
//
// A limit-stopped walk has fully walked its in-bounds slice, so it records
// collection completion (which lets the seal phase bundle that cold slice) but
// withholds settlement. Withholding settlement keeps the early stop disabled
// (the stop needs both), so a later walk with a wider (or no) limit still pages
// down to its own boundary rather than mistaking the excluded tail for settled
// history. A zero count and a zero oldest leave the walk unbounded, the
// default. It returns a [WalkOption].
func WithHistoryLimit(count int, oldest time.Time) WalkOption {
	return func(c *walkConfig) {
		c.historyCount = count
		c.historyOldest = oldest
	}
}

// Walk archives an append-mostly collection newest-first and halts as soon as it
// reaches already-archived history, the incremental re-run engine shared by every
// ordered collection (state versions, runs, per-run children, config versions).
//
// It pages through page newest-first, and for each element: advances the
// collection's high-water mark toward the element's CreatedAt, archives
// the element, and, once it archives an element that was already recorded done
// and is now terminal, stops. It archives that boundary element before stopping
// so a run that has just transitioned from running to finished still gets the
// final refresh of its mutable tail; a still-running element is re-archived and
// the walk continues, while brand-new elements append. Immutable children the
// element's Archive closure writes through [Env.Object], [Env.Blob], or
// [Env.Bytes] are skipped on that boundary refresh because the ledger already has
// them settled.
//
// The early stop is gated so it fires only once the collection is fully settled,
// because elements do not settle in creation order. A run is recorded done while
// still non-terminal and its children are deferred, and runs finish out of
// order, so a newer run reaching a terminal state can freeze the boundary ahead
// of an older run still in flight; and an older errored or forbidden child can
// sit below a newer done boundary. Three conditions compose to keep either from
// stranding it:
//
//   - the collection was walked to its end in a prior run
//     ([manifest.Collection.Complete]), so an interrupted first walk's
//     un-archived older tail is not mistaken for settled history;
//   - the newest full walk saw only terminal elements
//     ([manifest.Collection.Settled]), which carries a still-running run
//     (recorded done, so invisible to a status scan) across passes; and
//   - no errored or forbidden child sits below the boundary
//     ([manifest.Collection.HasUnsettled]), which forces a full re-walk while
//     any such child exists.
//
// A non-terminal element seen mid-pass suppresses the stop for that pass too, so
// the stop condition is strictly stricter than a plain complete check and can
// only ever widen the walk, never newly strand an element. When early-stop is
// suppressed the walk re-pages the collection: the immutable-object primitives
// still skip settled entries, so re-paging is near free for a workspace whose
// children are immutable, but an element that re-reads mutable metadata through
// [Env.Mutable] (a stack configuration's or step's diagnostics) re-fetches it
// every pass. On the final page it records both completion and whether this walk
// settled the collection.
//
// Settlement is withdrawn before it is re-earned: the walk records the
// collection unsettled before archiving its first not-already-frozen element,
// and records it settled again only on the paths that finished the walk's work
// (the true end of the listing, or an early stop whose new prefix archived
// clean). An interrupted re-walk of a settled collection (a failed page fetch,
// a cancellation, a kill) therefore leaves the flag false, and the next run
// pages past the interrupted walk's new entries instead of early-stopping
// above elements it never listed; those elements leave no ledger record at
// all, so the flag is the only guard. The ledger drains the
// false record ahead of the entries it guards in the flushed batch (see
// [manifest.Collection.SetSettled]), so no crash point persists the entries
// without it.
//
// The col handle carries every piece of the collection's ledger state
// (completion, settlement, the errored-child gate, and the high-water mark)
// keyed on the collection's archive prefix, so the flags live in the shard
// that owns the entries by construction (see [manifest.Collection] and
// [Env.Collection]). A collection listed through a synthetic cursor (a
// stack's configurations, a deployment group's runs) simply opens the handle
// on the directory its entries archive into; the cursor id names nothing in
// the ledger.
//
// A history limit ([WithHistoryLimit]) bounds the walk to the newest slice of
// the collection: the first listed element outside every configured bound ends
// the walk before that element archives. Completion is recorded once that
// in-bounds slice is fully walked, so the seal phase can bundle it, but
// settlement is withheld, so the early stop stays disabled and a later wider
// limit still pages down into the excluded tail. The count bound counts
// distinct elements: a collection that gains an element between two page
// fetches re-lists the previous page's last element, and spending a position on
// that duplicate would stop the walk short of the configured count, so an
// already-walked element is skipped rather than counted twice.
//
// Within a page the elements' Archive closures run concurrently, so any free
// worker can serve a large collection rather than one walking it alone; the
// shared client's request gate bounds the real parallelism, and the next page
// is not fetched until the current page's elements land, which keeps the
// early-stop decision and memory use those of the sequential walk. The stop
// boundary itself is decided in listing order from the ledger before the
// page's closures run, which matches the sequential decision because each
// element's frozen state depends only on its own entry, never on a page
// sibling's archive.
//
// Only a context cancellation surfaced by an Archive closure or a page fetch
// aborts the walk; every per-object error is recorded by the primitives and the
// walk continues.
func Walk[T any](
	ctx context.Context,
	env *Env,
	col *manifest.Collection,
	page Pager[T],
	describe func(T) Item,
	opts ...WalkOption,
) error {
	var cfg walkConfig

	for _, opt := range opts {
		opt(&cfg)
	}

	earlyStopAllowed := col.Complete() && col.Settled() && !col.HasUnsettled()

	sawNonTerminal := false
	outOfBounds := false
	unsettled := false
	listedCount := 0

	// A collection that gains an element between two page fetches shifts every
	// listed element one place older, so the next page re-lists the previous
	// page's last element. Counting that duplicate again would spend a position
	// in the count bound on an element already walked and stop the walk one
	// element short of the window the caller asked for, so a re-listed element is
	// skipped: it has already been counted, archived, and folded into the mark.
	// The set is kept only for a count-bounded walk, the one place a listing
	// position decides anything; an unbounded walk pages to the end of the
	// listing either way and pays no memory to remember it.
	var walked map[string]struct{}

	if cfg.historyCount > 0 {
		walked = make(map[string]struct{})
	}

	for pageNum := 1; ; pageNum++ {
		items, hasNext, err := page(ctx, pageNum)
		if err != nil {
			return fmt.Errorf("list %q page %d: %w", col.Prefix(), pageNum, err)
		}

		// Decide the page's boundary in listing order first: include each element
		// up to and including the first frozen one the settled gate lets the walk
		// stop on, exactly the elements the sequential walk would have archived.
		// The boundary element is still archived so a run that has just settled
		// gets its final refresh.
		include := make([]Item, 0, len(items))
		stopped := false
		pageMutates := false

		for _, listed := range items {
			item := describe(listed)

			// An element this walk already handled is a page-overlap duplicate,
			// not a position of its own.
			if _, dup := walked[item.RelPath]; dup {
				continue
			}

			// The first element outside every configured history bound starts the
			// excluded tail; it is not archived and does not advance the mark.
			if cfg.bounded() && !cfg.withinBounds(listedCount, item.CreatedAt) {
				outOfBounds = true

				break
			}

			listedCount++

			if walked != nil {
				walked[item.RelPath] = struct{}{}
			}

			col.AdvanceHighWaterMark(item.CreatedAt)

			entry, ok := env.ledger.Entry(item.RelPath)
			frozen := ok && entry.Status == manifest.StatusDone && item.Terminal

			include = append(include, item)

			if !frozen {
				pageMutates = true
			}

			if !item.Terminal {
				sawNonTerminal = true
			}

			if frozen && earlyStopAllowed && !sawNonTerminal {
				stopped = true

				break
			}
		}

		// A page about to record its first not-already-frozen element unsettles
		// the collection first, so an abort at any later point (a failed page
		// fetch, a cancellation, a kill mid-flush) leaves the flag false and
		// the next run pages past the new entries instead of early-stopping
		// above elements this walk never listed. The stale flag is the only
		// guard against that gap: an unlisted element leaves no ledger record
		// for the unsettled-child scan to find. A walk whose every element was
		// already frozen mutates no boundary and leaves the flag untouched.
		if pageMutates && !unsettled && col.Settled() {
			col.SetSettled(false)
		}

		unsettled = unsettled || pageMutates

		// Archive the included elements concurrently; the elements' paths are
		// distinct, so concurrent archives never contend on one ledger entry.
		g, gctx := errgroup.WithContext(ctx)

		for _, item := range include {
			g.Go(func() error {
				return item.Archive(gctx)
			})
		}

		err = g.Wait()
		if err != nil {
			return err //nolint:wrapcheck // Archive closures already return contextual errors.
		}

		if stopped {
			// A stop that archived new elements re-earns the settlement it
			// withdrew above: the boundary's history was settled (the stop was
			// allowed at all), the new prefix is archived and saw only terminal
			// elements (the stop requires that), and the unsettled-child scan
			// catches any of its archives that errored. Without this the next
			// run would re-page the whole collection after every delta.
			if unsettled {
				col.SetSettled(!sawNonTerminal && !col.HasUnsettled())
			}

			return nil
		}

		// A page with no elements is the end of the listing. A well-behaved pager
		// reports that with hasNext false, settled by the block below; guard
		// against a misbehaving pager that keeps advertising a next page over an
		// empty one, which would otherwise spin this loop with no progress.
		if len(items) == 0 {
			col.MarkComplete()
			col.SetSettled(!sawNonTerminal && !col.HasUnsettled())

			return nil
		}

		// A limit stop has fully walked its in-bounds slice, so it records
		// completion: the seal phase bundles a collection's cold artifacts only
		// once it is complete, and the excluded tail is left loose regardless. It
		// withholds settlement, which stays reserved for a walk that reached the
		// true end; leaving it unset keeps the early stop disabled, since that
		// needs both completion and settlement, so a later wider limit still pages
		// down into the tail rather than mistaking it for settled history.
		if outOfBounds {
			col.MarkComplete()

			return nil
		}

		if !hasNext {
			col.MarkComplete()
			col.SetSettled(!sawNonTerminal && !col.HasUnsettled())

			return nil
		}
	}
}
