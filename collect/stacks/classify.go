package stacks

import (
	"strconv"

	tfe "github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/store"
)

// configTerminal reports whether a stack configuration has settled into a state
// that no longer changes, so its snapshot and children can be frozen.
func configTerminal(status tfe.StackConfigurationStatus) bool {
	switch status {
	case tfe.StackConfigurationStatusCompleted, tfe.StackConfigurationStatusFailed:
		return true
	case tfe.StackConfigurationStatusPending,
		tfe.StackConfigurationStatusQueued,
		tfe.StackConfigurationStatusPreparing:
		return false
	default:
		return false
	}
}

// runTerminal reports whether a stack deployment run has reached a terminal
// status, so its record and steps need no further refresh once archived.
func runTerminal(status tfe.DeploymentRunStatus) bool {
	switch status {
	case tfe.DeploymentRunStatusSucceeded,
		tfe.DeploymentRunStatusFailed,
		tfe.DeploymentRunStatusAbandoned:
		return true
	case tfe.DeploymentRunStatusPending,
		tfe.DeploymentRunStatusPreDeploying,
		tfe.DeploymentRunStatusPreDeployingPendingOperator,
		tfe.DeploymentRunStatusAcquiringLock,
		tfe.DeploymentRunStatusDeploying,
		tfe.DeploymentRunStatusDeployingPendingOperator:
		return false
	default:
		return false
	}
}

// generationName renders a stack state generation as the stem of its state
// file, the key the states collection orders and dedupes on.
func generationName(generation int) string {
	return strconv.Itoa(generation)
}

// configKey derives the high-water-mark key for a stack's configuration walk
// from the stack id, keeping each stack's incremental cursor distinct.
func configKey(stackID string) string {
	return "stacks/" + stackID + "/configurations"
}

// runKey derives the high-water-mark key for a deployment group's run walk from
// the group id, keeping each group's incremental cursor distinct.
func runKey(groupID string) string {
	return "stack-deployment-groups/" + groupID + "/runs"
}

// configArchivePrefix is the archive-relative directory holding a stack's
// configuration entries: the real path prefix the configurations walk gates its
// errored-child check on, distinct from the synthetic [configKey] cursor.
//
// The cursor routes to the org-root shard, but the entries live in the stack's
// own shard, so the walk must gate on this prefix rather than the cursor for
// [manifest.Ledger.HasUnsettledUnder] to scan the shard that holds them.
func configArchivePrefix(st *store.Store, project, stackName string) string {
	return st.Join(st.StackDir(project, stackName), "configurations")
}

// runArchivePrefix is the archive-relative directory holding a deployment
// group's run entries: the real path prefix the runs walk gates its
// errored-child check on, distinct from the synthetic [runKey] cursor.
func runArchivePrefix(st *store.Store, project, stackName, configID, groupID string) string {
	return st.Join(st.StackDeploymentGroupDir(project, stackName, configID, groupID), "runs")
}
