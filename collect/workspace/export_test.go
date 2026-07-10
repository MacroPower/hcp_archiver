package workspace

import (
	"context"

	"github.com/hashicorp/go-tfe"
)

// RunTerminal exposes runTerminal to the external test package.
func RunTerminal(status tfe.RunStatus) bool {
	return runTerminal(status)
}

// ArchiveStateVersion exposes archiveStateVersion to the external test package.
func (c *Collector) ArchiveStateVersion(
	ctx context.Context,
	project, ws string,
	sv *tfe.StateVersion,
) error {
	return c.archiveStateVersion(ctx, project, ws, sv)
}

// StateVersionTerminal exposes stateVersionTerminal to the external test package.
func StateVersionTerminal(status tfe.StateVersionStatus) bool {
	return stateVersionTerminal(status)
}

// HasNextPage exposes hasNextPage to the external test package.
func HasNextPage(p *tfe.Pagination) bool {
	return hasNextPage(p)
}
