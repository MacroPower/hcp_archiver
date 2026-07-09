package archiver

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hashicorp/go-tfe"
	"golang.org/x/sync/errgroup"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/collect/workspace"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// defaultProjectName labels a workspace whose project cannot be resolved, so it
// still lands under a project directory (a workspace with no explicit project
// belongs to the organization's default project).
const defaultProjectName = "Default Project"

// collectProjects archives every project in the organization and returns a map
// from project id to project name for resolving each workspace's project.
//
// Enumeration paginates through the shared limiter; each project is archived by
// the workspace collector's project method.
func (a *Archiver) collectProjects(
	ctx context.Context,
	env *collect.Env,
	orgName string,
	wsc *workspace.Collector,
) (map[string]string, error) {
	projects, err := tfeclient.Paginate(ctx, env.Client(),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.Project, *tfe.Pagination, error) {
			l, e := tc.Projects.List(ctx, orgName, &tfe.ProjectListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list projects: %w", e)
			}

			return l.Items, l.Pagination, nil
		})
	if err != nil {
		return nil, fmt.Errorf("paginate projects: %w", err)
	}

	names := make(map[string]string, len(projects))
	for _, p := range projects {
		names[p.ID] = p.Name

		err = wsc.CollectProject(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("collect project %q: %w", p.Name, err)
		}
	}

	return names, nil
}

// collectWorkspaces archives every workspace in the organization, fanning them
// across a bounded worker pool sized by the configured concurrency.
//
// Enumeration hydrates each workspace's project relation so its project name
// resolves from names. Work within one workspace is sequential; workspaces run
// concurrently. A worker returns non-nil only on a cancellation, which cancels
// the pool.
func (a *Archiver) collectWorkspaces(
	ctx context.Context,
	env *collect.Env,
	orgName string,
	wsc *workspace.Collector,
	names map[string]string,
) error {
	workspaces, err := tfeclient.Paginate(ctx, env.Client(),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.Workspace, *tfe.Pagination, error) {
			l, e := tc.Workspaces.List(ctx, orgName, &tfe.WorkspaceListOptions{
				ListOptions: o,
				Include:     []tfe.WSIncludeOpt{tfe.WSProject},
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list workspaces: %w", e)
			}

			return l.Items, l.Pagination, nil
		})
	if err != nil {
		return fmt.Errorf("paginate workspaces: %w", err)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(a.cfg.WorkspaceConcurrency)

	for _, ws := range workspaces {
		g.Go(func() error {
			err := wsc.CollectWorkspace(gctx, projectNameFor(names, ws), ws)
			if err != nil && gctx.Err() == nil {
				// Best-effort: a non-cancellation failure (e.g. a transient
				// list error) for one workspace is logged and does not cancel
				// the pool, so it never aborts the rest of the organization. A
				// re-run re-walks the workspace and picks up what it missed.
				a.logger.LogAttrs(gctx, slog.LevelError, "workspace_archive_error",
					slog.String("org", orgName),
					slog.String("workspace", ws.Name),
					slog.String("error", err.Error()),
				)

				return nil
			}

			if err != nil {
				return fmt.Errorf("collect workspace %q: %w", ws.Name, err)
			}

			return nil
		})
	}

	err = g.Wait()
	if err != nil {
		return fmt.Errorf("collect workspaces: %w", err)
	}

	return nil
}

// projectNameFor resolves the project name of ws from names, keyed on the
// workspace's hydrated project id. It falls back to the project relation's own
// name and, absent that, to [defaultProjectName], so every workspace nests
// under a project directory.
func projectNameFor(names map[string]string, ws *tfe.Workspace) string {
	if ws.Project == nil {
		return defaultProjectName
	}

	name, ok := names[ws.Project.ID]
	if ok && name != "" {
		return name
	}

	if ws.Project.Name != "" {
		return ws.Project.Name
	}

	return defaultProjectName
}
