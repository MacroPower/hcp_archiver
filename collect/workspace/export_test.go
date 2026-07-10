package workspace

import (
	"context"

	"github.com/hashicorp/go-tfe"
)

// RunTerminal exposes runTerminal to the external test package.
func RunTerminal(status tfe.RunStatus) bool {
	return runTerminal(status)
}

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

// ArchiveStateVersion exposes archiveStateVersion to the external test package.
func (c *Collector) ArchiveStateVersion(
	ctx context.Context,
	project, ws string,
	sv *tfe.StateVersion,
) error {
	return c.archiveStateVersion(ctx, project, ws, sv)
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

// StateVersionTerminal exposes stateVersionTerminal to the external test package.
func StateVersionTerminal(status tfe.StateVersionStatus) bool {
	return stateVersionTerminal(status)
}

// HasNextPage exposes hasNextPage to the external test package.
func HasNextPage(p *tfe.Pagination) bool {
	return hasNextPage(p)
}
