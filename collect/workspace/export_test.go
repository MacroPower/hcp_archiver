package workspace

import "github.com/hashicorp/go-tfe"

// RunTerminal exposes runTerminal to the external test package.
func RunTerminal(status tfe.RunStatus) bool {
	return runTerminal(status)
}

// HasNextPage exposes hasNextPage to the external test package.
func HasNextPage(p *tfe.Pagination) bool {
	return hasNextPage(p)
}
