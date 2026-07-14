package registry

import "context"

// CollectProviders exposes collectProviders to the external test package, so the
// provider version and platform archive paths can be exercised on their own.
func (c *Collector) CollectProviders(ctx context.Context) error {
	return c.collectProviders(ctx)
}
