package audit

import (
	"context"
	"time"

	tfe "github.com/hashicorp/go-tfe"
)

// Exposes unexported pure helpers for tests in the external package.
var (
	PageName        = pageName
	NewestTimestamp = newestTimestamp
)

// EventsAfter exposes eventsAfter to the external test package, taking the
// already-persisted events as a slice and folding them into the id set the walk
// carries.
func EventsAfter(items []*tfe.AuditTrail, since time.Time, archived []*tfe.AuditTrail) []*tfe.AuditTrail {
	ids := archivedIDs{}
	ids.record(archived)

	return eventsAfter(items, since, ids)
}

// CollectTrails exposes collectTrails to the external test package, so the
// walk's watermark and halt behavior can be exercised without the audit
// configuration surface.
func (c *Collector) CollectTrails(ctx context.Context) error {
	return c.collectTrails(ctx)
}
