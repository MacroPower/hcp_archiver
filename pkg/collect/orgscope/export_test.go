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

// CollectPolicySource exposes collectPolicySource to the external test package.
func (c *Collector) CollectPolicySource(ctx context.Context, policy *tfe.Policy) error {
	return c.collectPolicySource(ctx, policy)
}

// ArchivePolicySetVersions exposes archivePolicySetVersions to the external test
// package.
func (c *Collector) ArchivePolicySetVersions(ctx context.Context, set *tfe.PolicySet) error {
	return c.archivePolicySetVersions(ctx, set)
}

// ArchiveOAuthClient exposes archiveOAuthClient to the external test package.
func (c *Collector) ArchiveOAuthClient(ctx context.Context, client *tfe.OAuthClient) error {
	return c.archiveOAuthClient(ctx, client)
}

// ArchiveUser exposes archiveUser to the external test package.
func (c *Collector) ArchiveUser(ctx context.Context, u *tfe.User) error {
	return c.archiveUser(ctx, u)
}
