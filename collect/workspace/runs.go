package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
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

	// A run can be deleted upstream while the walk archives a page, which shifts
	// the listing under a page-number pager; the stable pager re-lists rather
	// than let the shift skip a run (see [stablePager]).
	pager := newStablePager(func(run *tfe.Run) string { return run.ID },
		func(ctx context.Context, page int) ([]*tfe.Run, *tfe.Pagination, error) {
			var list *tfe.RunList

			// DoRunsList, not Do: the runs list endpoint is metered in its own bucket
			// of 30 requests per minute, and a slot is held across the wait for that
			// bucket's token, so drawing this walk's slots from the general gate
			// would park one for seconds at a time.
			err := c.env.Client().DoRunsList(ctx, func(ctx context.Context, tc *tfe.Client) error {
				// Each request fetches the maximum page rather than the default 20:
				// a fifth of the spend from the walk's scarcest budget.
				l, e := tc.Runs.List(ctx, wsID, &tfe.RunListOptions{
					ListOptions: tfe.ListOptions{PageNumber: page, PageSize: tfeclient.MaxPageSize},
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
				return nil, nil, fmt.Errorf("list runs page %d: %w", page, err)
			}

			return list.Items, list.Pagination, nil
		})

	describe := func(run *tfe.Run) collect.Item {
		return collect.Item{
			RelPath:   st.RunFile(project, wsName, run.ID, "run.json"),
			CreatedAt: run.CreatedAt,
			Terminal:  tfeclient.RunTerminal(run.Status),
			Archive: func(ctx context.Context) error {
				err := c.archiveRun(ctx, project, wsName, run)
				if err == nil && progress != nil {
					progress(1)
				}

				return err
			},
		}
	}

	return wrapArchive(key, collect.Walk(ctx, c.env, c.env.Collection(key), pager.page, describe,
		collect.WithHistoryLimit(c.runHistoryCount, c.runHistoryOldest)))
}

// archiveRun archives a run's mutable summary and, once the run is terminal, its
// immutable children.
func (c *Collector) archiveRun(ctx context.Context, project, ws string, run *tfe.Run) error {
	err := c.mutable(ctx, c.env.Store().RunFile(project, ws, run.ID, "run.json"), run)
	if err != nil {
		return err
	}

	if !tfeclient.RunTerminal(run.Status) {
		return nil
	}

	return c.archiveRunChildren(ctx, project, ws, run)
}

// terminalRunFile reports whether the loose run.json at absPath records a
// terminal run status, the seal-time gate that decides whether a run's summary
// is frozen enough to coalesce. The ledger entry carries no run status, so the
// archived document itself ({"data":{"attributes":{"status":...}}}) is the
// only place to read it. A missing or unparseable file reports false, the safe
// direction: the summary stays loose and keeps refreshing.
func terminalRunFile(absPath string) bool {
	//nolint:gosec // The path is composed by the store from its archive root.
	data, err := os.ReadFile(absPath)
	if err != nil {
		return false
	}

	var doc struct {
		Data struct {
			Attributes struct {
				Status string `json:"status"`
			} `json:"attributes"`
		} `json:"data"`
	}

	err = json.Unmarshal(data, &doc)
	if err != nil {
		return false
	}

	return tfeclient.RunTerminal(tfe.RunStatus(doc.Data.Attributes.Status))
}
