package archiver

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hashicorp/go-tfe"
	"golang.org/x/sync/errgroup"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/collect/workspace"
	"go.jacobcolvin.com/hcp_archiver/progress"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// defaultProjectName labels a workspace whose project cannot be resolved, so it
// still lands under a project directory (a workspace with no explicit project
// belongs to the organization's default project).
const defaultProjectName = "Default Project"

// Phase names for the two collection surfaces that carry a determinate bar.
const (
	phaseProjects   = "projects"
	phaseWorkspaces = "workspaces"
)

// collectProjects archives every project in the organization and returns a map
// from project id to project name for resolving each workspace's project.
//
// Enumeration paginates through the shared limiter; each project is archived by
// the workspace collector's project method.
func (a *Archiver) collectProjects(
	ctx context.Context,
	env *collect.Env,
	reporter *progress.Reporter,
	orgName string,
	wsc *workspace.Collector,
) (map[string]string, error) {
	reporter.SetPhase(phaseProjects)
	reporter.SetTotal(-1)

	projects, err := tfeclient.Paginate(ctx, env.Client(),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.Project, *tfe.Pagination, error) {
			l, e := tc.Projects.List(ctx, orgName, &tfe.ProjectListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list projects: %w", e)
			}

			return l.Items, l.Pagination, nil
		})
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("paginate projects: %w", ctx.Err())
		}

		// Best-effort: a non-cancellation list failure is logged and does not
		// abort the organization, so the independent registry, stacks, and
		// audit surfaces still run. A re-run retries the projects.
		a.logger.LogAttrs(ctx, slog.LevelError, "project_list_error",
			slog.String("org", orgName),
			slog.String("error", err.Error()),
		)

		return map[string]string{}, nil
	}

	reporter.SetTotal(len(projects))

	names := make(map[string]string, len(projects))
	for _, p := range projects {
		names[p.ID] = p.Name

		err = wsc.CollectProject(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("collect project %q: %w", p.Name, err)
		}

		reporter.Advance(1)
	}

	return names, nil
}

// collectWorkspaces archives every workspace in the organization, fanning them
// across the run's shared worker pool.
//
// Enumeration hydrates each workspace's project relation so its project name
// resolves from names. The goroutine per workspace is only a coordinator: it
// holds no worker slot itself, and every request it causes takes one from the
// client's gate, so slots flow across workspace boundaries — many small
// workspaces at once, or many workers inside one large workspace — and the
// pool's live size, not this fan-out, bounds the real parallelism. The
// fan-out is capped at the pool's ceiling so the in-flight task list stays
// meaningful. A workspace goroutine returns non-nil only on a cancellation,
// which cancels the group.
func (a *Archiver) collectWorkspaces(
	ctx context.Context,
	env *collect.Env,
	reporter *progress.Reporter,
	orgName string,
	wsc *workspace.Collector,
	names map[string]string,
) error {
	// Enter the phase indeterminate: the (possibly multi-page) listing and the
	// per-workspace counting pass below have no count yet, so the bar is a
	// spinner rather than the projects phase's stale full bar until the
	// weighted total is known. The target clears for the whole phase:
	// workspaces archive concurrently, so no single name is the target, and the
	// per-task progress names each in-flight workspace. Clearing also keeps the
	// projects phase's last target from lingering into this phase and beyond.
	reporter.SetPhase(phaseWorkspaces)
	reporter.SetTotal(-1)
	env.SetTarget("")

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
		if ctx.Err() != nil {
			return fmt.Errorf("paginate workspaces: %w", ctx.Err())
		}

		// Best-effort: a non-cancellation list failure is logged and does not
		// abort the organization, so the remaining collectors still run. A
		// re-run retries the workspaces.
		a.logger.LogAttrs(ctx, slog.LevelError, "workspace_list_error",
			slog.String("org", orgName),
			slog.String("error", err.Error()),
		)

		return nil
	}

	// Weight each workspace by 1 (its settings) + its probed run and
	// state-version counts so the bar tracks real work (per-workspace effort
	// spans orders of magnitude) and, since numerator and denominator share the
	// weight, reaches exactly 100% as the last workspace finishes. The probes
	// read each collection's listed total rather than the workspace's advertised
	// RunsCount, which omits speculative runs and can go stale; an underestimate
	// would peg the bar at 100% while the excess work drains. The phase total is
	// still -1 here, so the spinner covers the counting stretch.
	weights := make([]int, len(workspaces))

	var counters errgroup.Group

	counters.SetLimit(a.cfg.MaxConcurrency)

	for i, ws := range workspaces {
		counters.Go(func() error {
			runs, svs := wsc.Counts(ctx, ws)
			weights[i] = 1 + runs + svs

			return nil
		})
	}

	// The counting workers never return an error; each probe falls back to its
	// own estimate inside Counts.
	_ = counters.Wait() //nolint:errcheck // Always nil.

	// A cancellation mid-count leaves fallback weights behind; stop here rather
	// than registering tasks for work that will never run.
	if ctx.Err() != nil {
		return fmt.Errorf("count workspaces: %w", ctx.Err())
	}

	totalWeight := 0
	for _, w := range weights {
		totalWeight += w
	}

	reporter.SetTotal(totalWeight)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(a.cfg.MaxConcurrency)

	for i, ws := range workspaces {
		g.Go(func() error {
			project := projectNameFor(names, ws)

			// Track the workspace as a task so the panel can show progress inside
			// it, and so each unit archived advances the phase bar as it lands
			// rather than the whole weight arriving when the workspace finishes.
			// The name matches the target's project/workspace form. Done commits
			// any remainder on every return path (a failed workspace, or a walk
			// that stopped early on settled history), so the bar still reaches
			// 100%; deferring keeps that guarantee structural rather than
			// dependent on statement order above later returns.
			task := reporter.StartTask(project+"/"+ws.Name, weights[i])
			defer task.Done()

			err := wsc.CollectWorkspace(gctx, project, ws, task.Advance)
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

			// Seal the workspace's now-frozen cold artifacts into bundles. It runs
			// only after a clean collection, so the collections are complete; a
			// failure is logged and does not abort the pool, since the loose
			// sources stay canonical until a bundle verifies and a re-run re-seals.
			err = wsc.SealWorkspace(gctx, project, ws.Name)
			if err != nil && gctx.Err() == nil {
				a.logger.LogAttrs(gctx, slog.LevelError, "workspace_seal_error",
					slog.String("org", orgName),
					slog.String("workspace", ws.Name),
					slog.String("error", err.Error()),
				)

				return nil
			}

			if err != nil {
				return fmt.Errorf("seal workspace %q: %w", ws.Name, err)
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
