package stacks

import (
	"context"

	tfe "github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/store"
)

// ConfigTerminalForTest exposes [configTerminal] to the external test package.
func ConfigTerminalForTest(status tfe.StackConfigurationStatus) bool {
	return configTerminal(status)
}

// RunTerminalForTest exposes [runTerminal] to the external test package.
func RunTerminalForTest(status tfe.DeploymentRunStatus) bool {
	return runTerminal(status)
}

// StateTerminalForTest exposes [stateTerminal] to the external test package.
func StateTerminalForTest(status string) bool {
	return stateTerminal(status)
}

// GenerationNameForTest exposes [generationName] to the external test package.
func GenerationNameForTest(generation int) string {
	return generationName(generation)
}

// ConfigKeyForTest exposes [configKey] to the external test package.
func ConfigKeyForTest(stackID string) string {
	return configKey(stackID)
}

// RunKeyForTest exposes [runKey] to the external test package.
func RunKeyForTest(groupID string) string {
	return runKey(groupID)
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
