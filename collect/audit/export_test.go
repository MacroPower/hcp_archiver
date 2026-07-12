package audit

import "context"

// Exposes unexported pure helpers for tests in the external package.
var (
	PageName        = pageName
	NewestTimestamp = newestTimestamp
)

// CollectTrails exposes collectTrails to the external test package, so the
// walk's watermark and halt behavior can be exercised without the audit
// configuration surface.
func (c *Collector) CollectTrails(ctx context.Context) error {
	return c.collectTrails(ctx)
}
