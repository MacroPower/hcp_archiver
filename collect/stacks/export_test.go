package stacks

import (
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
