package workspace

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect"
)

// collectRuns archives the workspace's runs newest-first. A run's summary is
// mutable and refreshes while the run is in flight; its children are immutable
// and are fetched only once the run reaches a terminal state, so an in-flight
// run does not record premature absences for logs it has yet to produce. The
// progress callback, when non-nil, is called with 1 after each run is handled,
// including a settled run the primitives skip, so progress tracks the walk
// itself rather than only fresh downloads. A configured run-history limit
// ([WithRunHistoryLimit]) passes through to the walk, which stops at the first
// run outside every configured bound.
func (c *Collector) collectRuns(ctx context.Context, project string, ws *tfe.Workspace, progress func(n int)) error {
	st := c.env.Store()
	key := st.Join(st.WorkspaceDir(project, ws.Name), "runs")
	wsID := ws.ID
	wsName := ws.Name

	pager := func(ctx context.Context, page int) ([]*tfe.Run, bool, error) {
		var list *tfe.RunList

		err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
			l, e := tc.Runs.List(ctx, wsID, &tfe.RunListOptions{
				ListOptions: tfe.ListOptions{PageNumber: page},
				Include: []tfe.RunIncludeOpt{
					tfe.RunPlan,
					tfe.RunApply,
					tfe.RunConfigVer,
					tfe.RunCreatedBy,
					tfe.RunCostEstimate,
				},
			})
			list = l

			if e != nil {
				return fmt.Errorf("list runs: %w", e)
			}

			return nil
		})
		if err != nil {
			return nil, false, fmt.Errorf("list runs page %d: %w", page, err)
		}

		return list.Items, hasNextPage(list.Pagination), nil
	}

	describe := func(run *tfe.Run) collect.Item {
		return collect.Item{
			RelPath:   st.RunFile(project, wsName, run.ID, "run.json"),
			CreatedAt: run.CreatedAt,
			Terminal:  runTerminal(run.Status),
			Archive: func(ctx context.Context) error {
				err := c.archiveRun(ctx, project, wsName, run)
				if err == nil && progress != nil {
					progress(1)
				}

				return err
			},
		}
	}

	return wrapArchive(key, collect.Walk(ctx, c.env, key, pager, describe,
		collect.WithHistoryLimit(c.runHistoryCount, c.runHistoryOldest)))
}

// archiveRun archives a run's mutable summary and, once the run is terminal, its
// immutable children.
func (c *Collector) archiveRun(ctx context.Context, project, ws string, run *tfe.Run) error {
	err := c.mutable(ctx, c.env.Store().RunFile(project, ws, run.ID, "run.json"), run)
	if err != nil {
		return err
	}

	if !runTerminal(run.Status) {
		return nil
	}

	return c.archiveRunChildren(ctx, project, ws, run)
}

// runTerminal reports whether a run has settled into a final state and so needs
// no further refresh. The remaining statuses are in-flight or paused stages that
// may still advance.
func runTerminal(status tfe.RunStatus) bool {
	switch status {
	case tfe.RunApplied,
		tfe.RunPlannedAndFinished,
		tfe.RunDiscarded,
		tfe.RunErrored,
		tfe.RunCanceled:
		return true
	case tfe.RunStatus("force_canceled"):
		return true
	default:
		return false
	}
}
