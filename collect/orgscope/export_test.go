package orgscope

import (
	"context"

	"github.com/hashicorp/go-tfe"
)

// PolicyExt exposes the unexported policyExt helper to the external test
// package.
var PolicyExt = policyExt

// ArchiveHYOKConfiguration exposes archiveHYOKConfiguration to the external test
// package.
func (c *Collector) ArchiveHYOKConfiguration(ctx context.Context, config *tfe.HYOKConfiguration) error {
	return c.archiveHYOKConfiguration(ctx, config)
}

// ArchivePolicySetVersions exposes archivePolicySetVersions to the external test
// package.
func (c *Collector) ArchivePolicySetVersions(ctx context.Context, set *tfe.PolicySet) error {
	return c.archivePolicySetVersions(ctx, set)
}
