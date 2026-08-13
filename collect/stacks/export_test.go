package stacks

import (
	"context"

	tfe "github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/store"
)

// StateTerminalForTest exposes [stateTerminal] to the external test package.
func StateTerminalForTest(status string) bool {
	return stateTerminal(status)
}

// GenerationNameForTest exposes [generationName] to the external test package.
func GenerationNameForTest(generation int) string {
	return generationName(generation)
}

// ConfigArchivePrefixForTest exposes [configArchivePrefix] to the external test
// package.
func ConfigArchivePrefixForTest(st *store.Store, project, stackName string) string {
	return configArchivePrefix(st, project, stackName)
}

// RunArchivePrefixForTest exposes [runArchivePrefix] to the external test
// package.
func RunArchivePrefixForTest(st *store.Store, project, stackName, configID, groupID string) string {
	return runArchivePrefix(st, project, stackName, configID, groupID)
}

// ListingLeafForTest exposes [listingLeaf] to the external test package.
const ListingLeafForTest = listingLeaf

// ResolveProjectForTest exposes [Collector.resolveProject] to the external test
// package, reporting the resolved project name and whether it is the project's
// display name rather than a stand-in for one.
func (c *Collector) ResolveProjectForTest(ctx context.Context, stack *tfe.Stack) (string, bool) {
	resolved := c.resolveProject(ctx, stack)

	return resolved.name, resolved.named
}

// ArchiveConfigurationForTest exposes [Collector.archiveConfiguration] to the
// external test package.
func (c *Collector) ArchiveConfigurationForTest(
	ctx context.Context,
	project, stackName string,
	cfg *tfe.StackConfiguration,
) error {
	return c.archiveConfiguration(ctx, project, stackName, cfg)
}

// ArchiveGroupForTest exposes [Collector.archiveGroup] to the external test
// package.
func (c *Collector) ArchiveGroupForTest(
	ctx context.Context,
	project, stackName, configID string,
	group *tfe.StackDeploymentGroup,
) error {
	return c.archiveGroup(ctx, project, stackName, configID, group)
}

// ArchiveRunForTest exposes [Collector.archiveRun] to the external test
// package.
func (c *Collector) ArchiveRunForTest(
	ctx context.Context,
	project, stackName, configID, groupID string,
	run *tfe.StackDeploymentRun,
) error {
	return c.archiveRun(ctx, project, stackName, configID, groupID, run)
}
