package workspace

import (
	"context"

	"github.com/hashicorp/go-tfe"
)

// ArchiveConfigurationVersion exposes archiveConfigurationVersion to the
// external test package.
func (c *Collector) ArchiveConfigurationVersion(
	ctx context.Context,
	project, ws string,
	run *tfe.Run,
) error {
	return c.archiveConfigurationVersion(ctx, project, ws, run)
}

// ArchivePlan exposes archivePlan to the external test package.
func (c *Collector) ArchivePlan(ctx context.Context, project, ws string, run *tfe.Run) error {
	return c.archivePlan(ctx, project, ws, run)
}

// ArchiveApply exposes archiveApply to the external test package.
func (c *Collector) ArchiveApply(ctx context.Context, project, ws string, run *tfe.Run) error {
	return c.archiveApply(ctx, project, ws, run)
}

// ArchiveTFPolicyOutcomes exposes archiveTFPolicyOutcomes to the external test
// package.
func (c *Collector) ArchiveTFPolicyOutcomes(ctx context.Context, project, ws string, run *tfe.Run) error {
	return c.archiveTFPolicyOutcomes(ctx, project, ws, run)
}

// ArchiveRunEvents exposes archiveRunEvents to the external test package.
func (c *Collector) ArchiveRunEvents(ctx context.Context, project, ws string, run *tfe.Run) error {
	return c.archiveRunEvents(ctx, project, ws, run)
}

// ArchiveRunChildren exposes archiveRunChildren to the external test package.
func (c *Collector) ArchiveRunChildren(ctx context.Context, project, ws string, run *tfe.Run) error {
	return c.archiveRunChildren(ctx, project, ws, run)
}

// ArchivePolicyChecks exposes archivePolicyChecks to the external test package.
func (c *Collector) ArchivePolicyChecks(ctx context.Context, project, ws string, run *tfe.Run) error {
	return c.archivePolicyChecks(ctx, project, ws, run)
}

// ArchiveStateVersion exposes archiveStateVersion to the external test package.
func (c *Collector) ArchiveStateVersion(
	ctx context.Context,
	project, ws string,
	sv *tfe.StateVersion,
) error {
	return c.archiveStateVersion(ctx, project, ws, sv)
}

// CollectRuns exposes collectRuns to the external test package.
func (c *Collector) CollectRuns(
	ctx context.Context,
	project string,
	ws *tfe.Workspace,
	progress func(n int),
) error {
	return c.collectRuns(ctx, project, ws, progress)
}

// CollectStateVersions exposes collectStateVersions to the external test
// package.
func (c *Collector) CollectStateVersions(
	ctx context.Context,
	project string,
	ws *tfe.Workspace,
	progress func(n int),
) error {
	return c.collectStateVersions(ctx, project, ws, progress)
}

// HasNextPage exposes hasNextPage to the external test package.
func HasNextPage(p *tfe.Pagination) bool {
	return hasNextPage(p)
}
