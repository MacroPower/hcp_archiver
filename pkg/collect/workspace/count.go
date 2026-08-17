package workspace

import (
	"context"

	"github.com/hashicorp/go-tfe"
)

// Counts reports the workspace's run and state-version totals, in that order,
// so the orchestrator can budget the workspace's progress weight.
//
// The state-version total comes from one PageSize-1 probe of its listing; the
// probe falls back to zero when it errors or returns no pagination, so counting
// never blocks archiving. The run total is the workspace's advertised
// RunsCount: the runs list endpoint is metered in its own
// 30-requests-per-minute bucket (see
// [go.jacobcolvin.com/hcp_archiver/pkg/tfeclient.DefaultRunsListRateLimit]), so a
// per-workspace probe would spend two seconds of the run walk's own budget on a
// count the workspace summary already carries. The advertised count omits
// speculative runs and can go stale, which skews only the progress weighting,
// never what the walk archives.
//
// A count-only run-history limit caps what the run walk will archive, so the
// run total is clamped to it. When an age bound is configured the advertised
// total stands, since the age window admits an unknown number of runs and an
// undercount would peg the progress bar at 100% while the excess work drains.
func (c *Collector) Counts(ctx context.Context, ws *tfe.Workspace) (int, int) {
	runs := ws.RunsCount
	svs := 0

	wsName := ws.Name
	org := c.org

	if c.runHistoryCount > 0 && c.runHistoryOldest.IsZero() && runs > c.runHistoryCount {
		runs = c.runHistoryCount
	}

	var svPg *tfe.Pagination

	// Go-tfe validates StateVersionListOptions, so the probe must carry the
	// organization and workspace filters the real pager sends; without them it
	// errors on every call and the fallback silently under-counts.
	err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
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
