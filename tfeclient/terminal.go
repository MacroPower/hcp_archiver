package tfeclient

import tfe "github.com/hashicorp/go-tfe"

// The terminality tables decide, once per resource, which API statuses are
// final: a terminal object freezes in the archive and is never refreshed,
// while a non-terminal one keeps its collection re-walking until it settles.
//
// The polarity is shared by every table and must never flip: only a status
// positively known to be final maps to true, and a status missing from its
// table reads as non-terminal, the safe direction. A live object mistaken for
// terminal freezes premature absences for children it has yet to produce —
// silent and irreversible — while a final object mistaken for live only costs
// re-fetches until the table is updated. An element's terminal status also
// says nothing about its subtree: a completed stack configuration's
// deployment runs keep executing, which the stacks collector carries through
// its obligation markers, not through these tables.
//
// Each table names every status its go-tfe type defines, true or false, and
// the completeness test fails when a dependency bump introduces one a table
// does not mention, forcing a deliberate decision instead of a silent
// non-terminal default that would go unnoticed.
var (
	// The runTerminality table classifies workspace run statuses.
	//
	// The policy_soft_failed status is final: a soft-mandatory policy failure
	// on a plan-only run, which no override can advance. The policy_override
	// status, by contrast, stays non-terminal because a user override moves it
	// on.
	runTerminality = map[tfe.RunStatus]bool{
		tfe.RunApplied:            true,
		tfe.RunCanceled:           true,
		tfe.RunDiscarded:          true,
		tfe.RunErrored:            true,
		tfe.RunPlannedAndFinished: true,
		tfe.RunPolicySoftFailed:   true,
		// The HCP Terraform API documents force_canceled as a final run state,
		// but go-tfe (v1.109.0) defines no constant for it, so the wire value
		// is spelled here directly.
		tfe.RunStatus("force_canceled"): true,

		tfe.RunApplying:                 false,
		tfe.RunApplyQueued:              false,
		tfe.RunConfirmed:                false,
		tfe.RunCostEstimated:            false,
		tfe.RunCostEstimating:           false,
		tfe.RunFetching:                 false,
		tfe.RunFetchingCompleted:        false,
		tfe.RunPending:                  false,
		tfe.RunPlanned:                  false,
		tfe.RunPlannedAndSaved:          false,
		tfe.RunPlanning:                 false,
		tfe.RunPlanQueued:               false,
		tfe.RunPolicyChecked:            false,
		tfe.RunPolicyChecking:           false,
		tfe.RunPolicyOverride:           false,
		tfe.RunPostPlanAwaitingDecision: false,
		tfe.RunPostPlanCompleted:        false,
		tfe.RunPostPlanRunning:          false,
		tfe.RunPostApplyRunning:         false,
		tfe.RunPostApplyCompleted:       false,
		tfe.RunPreApplyRunning:          false,
		tfe.RunPreApplyCompleted:        false,
		tfe.RunPrePlanCompleted:         false,
		tfe.RunPrePlanRunning:           false,
		tfe.RunQueuing:                  false,
		tfe.RunQueuingApply:             false,
	}

	// The stateVersionTerminality table classifies state-version statuses.
	stateVersionTerminality = map[tfe.StateVersionStatus]bool{
		tfe.StateVersionFinalized: true,
		tfe.StateVersionDiscarded: true,
		// A server that predates the status attribute omits it. Such a server
		// has no pending state: versions exist only once uploaded, so an empty
		// status is a finalized version, and treating it as live would re-walk
		// every collection forever without ever settling its metadata.
		tfe.StateVersionStatus(""): true,

		tfe.StateVersionPending: false,
	}

	// The stackConfigurationTerminality table classifies stack-configuration
	// statuses.
	//
	// A configuration is terminal once preparation finishes; its deployment
	// runs keep executing long after (see the comment above).
	stackConfigurationTerminality = map[tfe.StackConfigurationStatus]bool{
		tfe.StackConfigurationStatusCompleted: true,
		tfe.StackConfigurationStatusFailed:    true,

		tfe.StackConfigurationStatusPending:   false,
		tfe.StackConfigurationStatusQueued:    false,
		tfe.StackConfigurationStatusPreparing: false,
	}

	// The deploymentRunTerminality table classifies stack deployment-run
	// statuses.
	deploymentRunTerminality = map[tfe.DeploymentRunStatus]bool{
		tfe.DeploymentRunStatusSucceeded: true,
		tfe.DeploymentRunStatusFailed:    true,
		tfe.DeploymentRunStatusAbandoned: true,

		tfe.DeploymentRunStatusPending:                     false,
		tfe.DeploymentRunStatusPreDeploying:                false,
		tfe.DeploymentRunStatusPreDeployingPendingOperator: false,
		tfe.DeploymentRunStatusAcquiringLock:               false,
		tfe.DeploymentRunStatusDeploying:                   false,
		tfe.DeploymentRunStatusDeployingPendingOperator:    false,
	}
)

// RunTerminal reports whether a workspace run has settled into a final state
// and so needs no further refresh.
func RunTerminal(status tfe.RunStatus) bool {
	return runTerminality[status]
}

// StateVersionTerminal reports whether a state version has finished uploading,
// so its blobs and metadata can be fetched once and frozen.
func StateVersionTerminal(status tfe.StateVersionStatus) bool {
	return stateVersionTerminality[status]
}

// StackConfigurationTerminal reports whether a stack configuration has settled
// into a state that no longer changes, so its snapshot and children can be
// frozen.
func StackConfigurationTerminal(status tfe.StackConfigurationStatus) bool {
	return stackConfigurationTerminality[status]
}

// DeploymentRunTerminal reports whether a stack deployment run has reached a
// final status, so its record and steps need no further refresh once archived.
func DeploymentRunTerminal(status tfe.DeploymentRunStatus) bool {
	return deploymentRunTerminality[status]
}
