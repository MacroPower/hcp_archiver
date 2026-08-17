package archiver

import (
	"github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/pkg/namefilter"
)

// filterProjects returns the projects the configured project filter admits,
// preserving the listing's order. An empty filter admits every project.
func filterProjects(filter []string, projects []*tfe.Project) []*tfe.Project {
	f := namefilter.New(filter)
	if f == nil {
		return projects
	}

	kept := make([]*tfe.Project, 0, len(projects))

	for _, p := range projects {
		if f.Admits(p.Name) {
			kept = append(kept, p)
		}
	}

	return kept
}

// filterWorkspaces returns the workspaces the configured project and workspace
// filters admit, preserving the listing's order. A workspace passes when the
// project filter admits its project name (resolved through names exactly as
// its archive path is) and the workspace filter admits its own name; an empty
// filter admits everything on its axis.
func filterWorkspaces(
	projectFilter, workspaceFilter []string,
	names map[string]string,
	workspaces []*tfe.Workspace,
) []*tfe.Workspace {
	pf := namefilter.New(projectFilter)
	wf := namefilter.New(workspaceFilter)

	if pf == nil && wf == nil {
		return workspaces
	}

	kept := make([]*tfe.Workspace, 0, len(workspaces))

	for _, ws := range workspaces {
		if pf.Admits(projectNameFor(names, ws)) && wf.Admits(ws.Name) {
			kept = append(kept, ws)
		}
	}

	return kept
}

// unmatchedFilter returns the filter entries naming nothing the listing
// yielded, in the filter's order. The archiver warns on each so a typo in a
// filter surfaces rather than silently shrinking the archive.
func unmatchedFilter[T any](filter []string, items []T, name func(T) string) []string {
	if len(filter) == 0 {
		return nil
	}

	listed := make(map[string]struct{}, len(items))
	for _, item := range items {
		listed[name(item)] = struct{}{}
	}

	var missing []string

	for _, f := range filter {
		if _, ok := listed[f]; !ok {
			missing = append(missing, f)
		}
	}

	return missing
}
