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
	archivePrefix string
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
//   - [WithArchivePrefix]
//   - [WithHistoryLimit]
type WalkOption func(*walkConfig)

// WithArchivePrefix sets the archive-relative path prefix under which the
// collection's entries live, for use by the walk's errored-child gate when that
// prefix differs from the cursor key.
//
// A collection whose cursor key is itself a path prefix of its entries, such as
// a workspace's runs (projects/.../runs) or state versions, needs no override:
// the walk gates on the key directly. A collection cursored by a synthetic id
// whose entries live elsewhere, such as a stack's configurations or a deployment
// group's runs (keyed on an id but archived under projects/.../stacks/...),
// passes the real prefix here so [manifest.Ledger.HasUnsettledUnder] scans the
// shard that actually holds the entries and can find an errored or forbidden
// child left below a done boundary. An empty prefix keeps the default (the cursor
// key). It returns a [WalkOption].
func WithArchivePrefix(prefix string) WalkOption {
	return func(c *walkConfig) {
		if prefix != "" {
			c.archivePrefix = prefix
		}
	}
}

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
// The early stop is gated so it fires only once the collection is fully settled,
// because elements do not settle in creation order. A run is recorded done while
// still non-terminal and its children are deferred, and runs finish out of
// order, so a newer run reaching a terminal state can freeze the boundary ahead
// of an older run still in flight; and an older errored or forbidden child can
// sit below a newer done boundary. Three conditions compose to keep either from
// stranding it:
//
//   - the collection was walked to its end in a prior run
//     ([manifest.Ledger.IsCollectionComplete]), so an interrupted first walk's
//     un-archived older tail is not mistaken for settled history;
//   - the newest full walk saw only terminal elements
//     ([manifest.Ledger.IsCollectionSettled]), which carries a still-running run
//     (recorded done, so invisible to a status scan) across passes; and
//   - no errored or forbidden child sits below the boundary
//     ([manifest.Ledger.HasUnsettledUnder]), which forces a full re-walk while
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
// The errored-child gate scans the shard owning key, so it works directly for a
// collection whose key is a path prefix of its entries; a collection cursored by
// a synthetic id (a stack walk) passes the real archive prefix through
// [WithArchivePrefix] so the gate scans the right shard.
//
// A history limit ([WithHistoryLimit]) bounds the walk to the newest slice of
// the collection: the first listed element outside every configured bound ends
// the walk before that element archives. Completion is recorded once that
// in-bounds slice is fully walked, so the seal phase can bundle it, but
// settlement is withheld, so the early stop stays disabled and a later wider
// limit still pages down into the excluded tail.
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
	key string,
	page Pager[T],
	describe func(T) Item,
	opts ...WalkOption,
) error {
	cfg := walkConfig{archivePrefix: key}
	for _, opt := range opts {
		opt(&cfg)
	}

	earlyStopAllowed := env.ledger.IsCollectionComplete(key) &&
		env.ledger.IsCollectionSettled(key) &&
		!env.ledger.HasUnsettledUnder(cfg.archivePrefix)

	sawNonTerminal := false
	outOfBounds := false
	listedCount := 0

	for pageNum := 1; ; pageNum++ {
		items, hasNext, err := page(ctx, pageNum)
		if err != nil {
			return fmt.Errorf("list %q page %d: %w", key, pageNum, err)
		}

		// Decide the page's boundary in listing order first: include each element
		// up to and including the first frozen one the settled gate lets the walk
		// stop on, exactly the elements the sequential walk would have archived.
		// The boundary element is still archived so a run that has just settled
		// gets its final refresh.
		include := make([]Item, 0, len(items))
		stopped := false

		for _, listed := range items {
			item := describe(listed)

			// The first element outside every configured history bound starts the
			// excluded tail; it is not archived and does not advance the mark.
			if cfg.bounded() && !cfg.withinBounds(listedCount, item.CreatedAt) {
				outOfBounds = true

				break
			}

			listedCount++

			env.ledger.AdvanceHighWaterMark(key, item.CreatedAt)

			entry, ok := env.ledger.Entry(item.RelPath)
			frozen := ok && entry.Status == manifest.StatusDone && item.Terminal

			include = append(include, item)

			if !item.Terminal {
				sawNonTerminal = true
			}

			if frozen && earlyStopAllowed && !sawNonTerminal {
				stopped = true

				break
			}
		}

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
			return nil
		}

		// A page with no elements is the end of the listing. A well-behaved pager
		// reports that with hasNext false, settled by the block below; guard
		// against a misbehaving pager that keeps advertising a next page over an
		// empty one, which would otherwise spin this loop with no progress.
		if len(items) == 0 {
			env.ledger.MarkCollectionComplete(key)
			env.ledger.SetCollectionSettled(key, !sawNonTerminal && !env.ledger.HasUnsettledUnder(cfg.archivePrefix))

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
			env.ledger.MarkCollectionComplete(key)

			return nil
		}

		if !hasNext {
			env.ledger.MarkCollectionComplete(key)
			env.ledger.SetCollectionSettled(key, !sawNonTerminal && !env.ledger.HasUnsettledUnder(cfg.archivePrefix))

			return nil
		}
	}
}
