package audit

import (
	"fmt"
	"time"

	tfe "github.com/hashicorp/go-tfe"
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
// formats to a fixed epoch stamp. The page number is zero-padded so the names
// sort in page order.
func pageName(since time.Time, page int) string {
	stamp := since.UTC().Format(pageTimeLayout) + "Z"

	return fmt.Sprintf("%s-p%04d.json", stamp, page)
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
