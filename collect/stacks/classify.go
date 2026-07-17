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
	default:
		return false
	}
}

// stateTerminal reports whether a stack state generation has finished uploading,
// so its description can be fetched once and frozen.
//
// A generation is listable before its description exists: the
// /stack-states/:id/description endpoint answers 204 No Content "when state has
// not yet been uploaded" (and 404 while it is still computing), which the blob
// path settles as a permanent not-applicable gap or a sticky absence that a
// re-run never revisits, silently losing the finalized description once it
// uploads. Gating the fetch on a terminal status keeps the generation re-listed
// until it settles, mirroring the config, run, and state-version collectors.
//
// The go-tfe SDK types the status as a bare string with no constants, so the
// allowlist is spelled out here. The polarity matches every terminality predicate
// (see runTerminal, configTerminal, and workspace stateVersionTerminal): only a
// status positively known to be final is terminal, and an unrecognized one
// falls through to non-terminal, because mistaking a still-uploading generation
// for terminal settles a transient 204 as a permanent gap, silent and
// irreversible, while mistaking a final one for live only costs re-lists until
// this list is updated.
func stateTerminal(status string) bool {
	switch status {
	case "completed":
		// The one documented terminal status: the description has uploaded.
		return true
	case "":
		// A server that omits the status attribute reports no live phase for a
		// generation; treating an empty status as live would skip every
		// generation's description forever, so it is terminal, matching
		// stateVersionTerminal's handling of the same gap.
		return true
	default:
		return false
	}
}

// generationName renders a stack state generation as the stem of its state
// file, the key the states collection orders and dedupes on.
func generationName(generation int) string {
	return strconv.Itoa(generation)
}

// configArchivePrefix is the archive-relative directory holding a stack's
// configuration entries, the collection identity its walk opens the ledger
// handle on ([collect.Env.Collection]): the flags, the errored-child gate,
// and the high-water mark all key on it, so they live in the stack shard that
// owns the entries.
func configArchivePrefix(st *store.Store, project, stackName string) string {
	return st.Join(st.StackDir(project, stackName), "configurations")
}

// runArchivePrefix is the archive-relative directory holding a deployment
// group's run entries, the collection identity its walk opens the ledger
// handle on, so an errored step artifact below a done run boundary stays
// visible to the gate.
func runArchivePrefix(st *store.Store, project, stackName, configID, groupID string) string {
	return st.Join(st.StackDeploymentGroupDir(project, stackName, configID, groupID), "runs")
}
