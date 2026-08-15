package audit

import (
	"fmt"
	"time"

	tfe "github.com/hashicorp/go-tfe/v2"
)

// pageTimeLayout formats the Since cursor into a lexically-sortable,
// filesystem-safe stamp carrying no colons; paired with a literal "Z" it
// denotes UTC. The fixed-width fractional-seconds field preserves the cursor's
// sub-second precision: audit timestamps cluster within a wall-clock second in
// an active organization, so a whole-second stamp would let two runs whose
// watermarks share a second produce the same page name and settle the later
// run's events under a name the first run already recorded, silently losing
// them.
const pageTimeLayout = "20060102T150405.000000000"

// pageName builds the leaf name of an audit-trail page file from the Since
// cursor and the page number.
//
// The cursor stamp groups a single forward walk's pages and keeps them distinct
// from a later run's pages (which walk from an advanced cursor), so appending a
// page never collides with or clobbers an earlier run's immutable page. Two
// runs whose watermarks differ by any amount get distinct stamps, so the later
// run never reuses the earlier run's page names. The zero cursor of a first run
// formats to a fixed epoch stamp. The page number is zero-padded to nine digits
// so the names sort in page order for any realistic page count; a four-digit pad
// would let page 10000 sort before page 9999.
func pageName(since time.Time, page int) string {
	stamp := since.UTC().Format(pageTimeLayout) + "Z"

	return fmt.Sprintf("%s-p%09d.json", stamp, page)
}

// archivedIDs holds the ids of the audit events a walk has already persisted
// under its Since cursor, the second half of the freshness test [eventsAfter]
// applies.
//
// The cursor alone cannot express it: the watermark is a single instant, while a
// resumed walk carries per-page state -- the settled pages' stored events, which
// are strictly newer than the cursor and so pass the timestamp test even though
// they are already archived.
type archivedIDs map[string]struct{}

// record folds the events persisted for one page into the set. Events without an
// id are ignored: they cannot be matched against a re-listed event, and an empty
// key would suppress every other id-less event.
func (a archivedIDs) record(items []*tfe.AuditTrail) {
	for _, it := range items {
		if it != nil && it.ID != "" {
			a[it.ID] = struct{}{}
		}
	}
}

// holds reports whether the event id is already archived under the cursor.
func (a archivedIDs) holds(id string) bool {
	_, ok := a[id]

	return ok
}

// eventsAfter returns the items whose timestamp is strictly newer than since and
// that archived does not already hold, preserving the API's order.
//
// The API floors the Since cursor to whole-second precision on the wire (jsonapi
// renders a [time.Time] as RFC3339), so a walk resuming from a sub-second
// watermark re-lists the events already archived in the watermark's second.
// Dropping them keeps a page from duplicating a prior run's trailing second
// under a fresh page name.
//
// The archived set covers the other source of re-listed events: pagination
// shift. Events arriving between runs push the listing back a slot, so a page
// the walk has not settled yet lists events a settled sibling page already
// holds. They are strictly newer than the unadvanced cursor, so only their ids
// distinguish them from genuinely new events.
func eventsAfter(items []*tfe.AuditTrail, since time.Time, archived archivedIDs) []*tfe.AuditTrail {
	fresh := make([]*tfe.AuditTrail, 0, len(items))

	for _, it := range items {
		if it != nil && it.Timestamp.After(since) && !archived.holds(it.ID) {
			fresh = append(fresh, it)
		}
	}

	return fresh
}

// newestTimestamp returns the latest [tfe.AuditTrail.Timestamp] among items, or
// the zero time when items is empty. It feeds the watermark the walk advances
// to, independent of the order the API returns entries in.
func newestTimestamp(items []*tfe.AuditTrail) time.Time {
	var newest time.Time

	for _, it := range items {
		if it != nil && it.Timestamp.After(newest) {
			newest = it.Timestamp
		}
	}

	return newest
}
