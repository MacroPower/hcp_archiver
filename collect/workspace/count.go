package workspace

import (
	"context"

	"github.com/hashicorp/go-tfe"
)

// Counts reports the workspace's listed run and state-version totals, in that
// order, from one PageSize-1 probe of each collection, so the orchestrator can
// budget the workspace's progress weight from what the run and state-version
// walks will actually archive rather than the workspace's advertised RunsCount
// (which omits speculative runs and can go stale). Each probe falls back
// independently when it errors or returns no pagination, the run count to the
// advertised RunsCount and the state-version count to zero, so counting never
// blocks archiving.
func (c *Collector) Counts(ctx context.Context, ws *tfe.Workspace) (int, int) {
	runs := ws.RunsCount
	svs := 0

	wsID := ws.ID
	wsName := ws.Name
	org := c.org

	var runPg *tfe.Pagination

	err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		l, e := tc.Runs.List(ctx, wsID, &tfe.RunListOptions{
			ListOptions: tfe.ListOptions{PageSize: 1},
		})
		if e != nil {
			return e //nolint:wrapcheck // Only inspected for success.
		}

		runPg = l.Pagination

		return nil
	})
	if err == nil && runPg != nil {
		runs = runPg.TotalCount
	}

	var svPg *tfe.Pagination

	// Go-tfe validates StateVersionListOptions, so the probe must carry the
	// organization and workspace filters the real pager sends; without them it
	// errors on every call and the fallback silently under-counts.
	err = c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		l, e := tc.StateVersions.List(ctx, &tfe.StateVersionListOptions{
			ListOptions:  tfe.ListOptions{PageSize: 1},
			Organization: org,
			Workspace:    wsName,
		})
		if e != nil {
			return e //nolint:wrapcheck // Only inspected for success.
		}

		svPg = l.Pagination

		return nil
	})
	if err == nil && svPg != nil {
		svs = svPg.TotalCount
	}

	return runs, svs
}
