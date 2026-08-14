package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/go-tfe"
)

// ErrListingUnstable reports a listing that kept losing elements under a walk's
// pager: it shrank on every re-list the pager attempted, so the pager stopped
// rather than page over boundaries it can no longer trust. The walk aborts,
// which leaves the collection unsettled, and the next run lists it again from
// the head.
var ErrListingUnstable = errors.New("listing shrank repeatedly under the pager")

// maxResyncs bounds how many times one page request re-lists after the listing
// shrank under it. Each re-list answers a deletion that landed during that one
// request, so the bound is a liveness guard against a listing being edited
// continuously, not a tolerance the steady path spends.
const maxResyncs = 3

// scanSlack is the page allowance a re-list has beyond the pages already
// served: the page it lands on, plus one for a listing that grew at the head
// while it ran.
const scanSlack = 2

// listPage fetches one page of a newest-first listing, reporting the page's
// items and the server's pagination for it.
//
// See [stablePager] for the consumer.
type listPage[T any] func(ctx context.Context, page int) ([]T, *tfe.Pagination, error)

// stablePager adapts a page-number list endpoint into a [collect.Pager] that
// never silently skips an element when the live listing shifts between two page
// fetches.
//
// A page-number pager addresses elements by position, and [collect.Walk]
// archives a whole page (seconds to minutes of downloads) before it asks for the
// next one. Deleting an element upstream in that window shifts every later
// element up one position, so the element that would have led the next page
// falls into the range already fetched and is never listed at all. It gets no
// ledger entry, which leaves nothing for the unsettled-child scan to find, and
// the walk still records the collection complete and settled, so every later
// pass early-stops above the gap and the element is excluded permanently.
//
// The pager closes that gap by keeping its own cursor and its own record of
// what it has served:
//
//   - Elements only shift up when the listing loses more elements above the
//     boundary than it gained, and new elements only ever appear at the newest
//     end, so a total count that has dropped since the previous fetch is a
//     necessary condition for a skip. Every fetch compares against the previous
//     one, and any drop triggers a re-list; a drop with no skip behind it (a
//     deletion below the boundary) costs a re-list and nothing else.
//   - A re-list restarts the scan at the page holding the boundary the walk had
//     reached, less the elements the listing lost, since that is the earliest
//     position an unserved element can have shifted to. It then pages forward,
//     serving the elements it has not served yet.
//   - Every element is served at most once, by id. That is what makes a re-list
//     safe: re-serving an already-archived element would let the walk's early
//     stop fire on it, above the very gap the re-list exists to close. It also
//     absorbs the opposite shift, where an element created at the head pushes an
//     already-served one down into the next page.
//
// The record of served ids is one short string per listed element, held for the
// duration of the walk, which is the memory price of the guarantee.
//
// An endpoint that reports no total leaves the pager nothing to compare, so a
// shift there goes undetected; both listings walked through it report one.
//
// Create instances with [newStablePager].
type stablePager[T any] struct {
	fetch  listPage[T]
	id     func(T) string
	served map[string]struct{}
	next   int
	total  int
	span   int
	sized  bool
}

// newStablePager creates a new [stablePager] over fetch, identifying the
// listing's elements by id.
func newStablePager[T any](id func(T) string, fetch listPage[T]) *stablePager[T] {
	p := &stablePager[T]{fetch: fetch, id: id}
	p.reset()

	return p
}

// page reports the next chunk of the listing, a [collect.Pager] over the pager's
// fetch closure.
//
// The page number is the walk's own count, not the endpoint's: the pager holds
// its own cursor so a re-list can drop back to an earlier page without the walk
// knowing, and the chunk it answers with is simply the next run of elements the
// walk has not seen. A request for page 1 starts a fresh walk of the listing.
//
// It answers with no items only at the end of the listing: a chunk that filtered
// down to nothing is never reported as an empty page, since [collect.Walk] reads
// an empty page as the end.
func (p *stablePager[T]) page(ctx context.Context, page int) ([]T, bool, error) {
	if page <= 1 {
		p.reset()
	}

	resyncs, scanned := 0, 0

	for {
		if scanned >= p.scanBudget() {
			return nil, false, fmt.Errorf("%w: %d pages scanned reaching page %d", ErrListingUnstable, scanned, p.next)
		}

		items, pg, err := p.fetch(ctx, p.next)
		if err != nil {
			//nolint:wrapcheck // The fetch closure already names the listing and the page.
			return nil, false, err
		}

		scanned++

		lost := p.lost(pg)
		p.measure(pg, items, hasNextPage(pg))

		if lost > 0 {
			resyncs++
			if resyncs > maxResyncs {
				return nil, false, fmt.Errorf("%w: %d elements gone across %d re-lists",
					ErrListingUnstable, lost, resyncs)
			}

			scanned = 0
			p.next = p.resyncPage(lost)

			continue
		}

		// An empty page is the end of the listing, the rule [collect.Walk]
		// applies to an endpoint that keeps advertising a further page over one.
		if len(items) == 0 {
			return nil, false, nil
		}

		fresh := p.take(items)
		p.next++

		if len(fresh) > 0 {
			return fresh, hasNextPage(pg), nil
		}

		// Every element on this page has been served already: the prefix a
		// re-list pages back over. Skip it rather than answer with it, since
		// re-serving frozen elements would trip the walk's early stop above the
		// gap, and an empty answer would end the walk outright.
		if !hasNextPage(pg) {
			return nil, false, nil
		}
	}
}

// reset starts a fresh walk of the listing, dropping the cursor and the served
// ids of the previous one.
func (p *stablePager[T]) reset() {
	p.served = make(map[string]struct{})
	p.next = 1
	p.total = 0
	p.span = 0
	p.sized = false
}

// take reports the page's elements the walk has not been served yet, in listing
// order, recording them as served. An element the id function cannot name is
// always served: without an identity it cannot be recognized on a re-list, and
// serving it twice is recoverable where dropping it is the gap this pager exists
// to close.
func (p *stablePager[T]) take(items []T) []T {
	fresh := make([]T, 0, len(items))

	for _, item := range items {
		id := p.id(item)

		if id == "" {
			fresh = append(fresh, item)

			continue
		}

		_, seen := p.served[id]
		if seen {
			continue
		}

		p.served[id] = struct{}{}

		fresh = append(fresh, item)
	}

	return fresh
}

// lost reports how many elements the listing has shed since the last measured
// fetch: zero when it grew, held steady, or reported no total to compare.
func (p *stablePager[T]) lost(pg *tfe.Pagination) int {
	total, ok := listingTotal(pg)
	if !ok || p.total == 0 || total >= p.total {
		return 0
	}

	return p.total - total
}

// measure records what the fetched page reveals about the listing: the element
// count the next fetch compares against, and the endpoint's page size, which
// only a page with a further page after it reports honestly. The smallest such
// page is kept, the conservative reading, since underestimating the page size
// only ever sends a re-list further back than it needed to go. A page that
// reports no total leaves the previous measurement standing rather than reading
// the silence as an empty listing.
func (p *stablePager[T]) measure(pg *tfe.Pagination, items []T, hasNext bool) {
	if hasNext && len(items) > 0 && (!p.sized || len(items) < p.span) {
		p.span = len(items)
		p.sized = true
	}

	total, ok := listingTotal(pg)
	if !ok {
		return
	}

	p.total = total
}

// resyncPage reports the page a re-list restarts its scan from. Elements shift
// up by at most the number the listing lost, so the first element the walk has
// not served sits no earlier than that many positions before the boundary it had
// reached. A listing whose page size is still unknown is scanned from the head.
func (p *stablePager[T]) resyncPage(lost int) int {
	if !p.sized {
		return 1
	}

	back := max(len(p.served)-lost, 0)

	return back/p.span + 1
}

// scanBudget reports how many pages one request may fetch: the pages already
// served, which a re-list scans back over, plus [scanSlack]. A listing whose
// page size is still unknown budgets one page per served element, the permissive
// reading, since the budget guards only against an endpoint that pages forever,
// never against a listing legitimately re-scanned.
func (p *stablePager[T]) scanBudget() int {
	if !p.sized {
		return len(p.served) + scanSlack
	}

	return len(p.served)/p.span + scanSlack
}

// listingTotal reads the element count a page's pagination reports, and whether
// it reported one at all: an endpoint that omits the total serves it as zero,
// which is indistinguishable from an empty listing and so is no baseline.
func listingTotal(pg *tfe.Pagination) (int, bool) {
	if pg == nil || pg.TotalCount <= 0 {
		return 0, false
	}

	return pg.TotalCount, true
}

// hasNextPage reports whether pagination points at a further page, tolerating a
// nil pagination from an endpoint that omits it.
func hasNextPage(p *tfe.Pagination) bool {
	return p != nil && p.NextPage != 0
}
