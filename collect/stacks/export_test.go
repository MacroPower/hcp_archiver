package stacks

import tfe "github.com/hashicorp/go-tfe"

// ConfigTerminalForTest exposes [configTerminal] to the external test package.
func ConfigTerminalForTest(status tfe.StackConfigurationStatus) bool {
	return configTerminal(status)
}

// RunTerminalForTest exposes [runTerminal] to the external test package.
func RunTerminalForTest(status tfe.DeploymentRunStatus) bool {
	return runTerminal(status)
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
