package archiver

import (
	"context"
	"fmt"
	"time"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/collect/audit"
	"go.jacobcolvin.com/hcp_archiver/collect/orgscope"
	"go.jacobcolvin.com/hcp_archiver/collect/registry"
	"go.jacobcolvin.com/hcp_archiver/collect/stacks"
	"go.jacobcolvin.com/hcp_archiver/collect/workspace"
	"go.jacobcolvin.com/hcp_archiver/progress"
)

// collectOrg runs the domain collectors against one organization in order.
//
// Each collector is best-effort: a single missing or failed object is recorded
// and the walk continues, so a collector returns non-nil only on a cancellation
// of ctx, which stops the whole organization. A listing a collector had to drop
// is recorded in the ledger as a dropped surface ([collect.Env.MarkSurfaceDropped])
// rather than returned, so the remaining collectors still run and the run's
// close derives incompleteness from the tally in one place. It threads reporter
// through so each step names its phase and, where a cheap pre-count exists
// (projects, workspaces), drives a determinate bar. Optional surfaces run only
// when their configuration toggle is set.
func (a *Archiver) collectOrg(
	ctx context.Context,
	env *collect.Env,
	reporter *progress.Reporter,
	orgName string,
) error {
	err := a.runCollector(ctx, reporter, orgscope.New(env, orgName, orgscope.WithHYOK(a.cfg.HYOK)))
	if err != nil {
		return err
	}

	wsc := workspace.New(env, orgName,
		workspace.WithRunHistoryLimit(a.cfg.RunHistoryCount, a.runHistoryOldest()))

	projectNames, err := a.collectProjects(ctx, env, reporter, orgName, wsc)
	if err != nil {
		return err
	}

	err = a.collectWorkspaces(ctx, env, reporter, orgName, wsc, projectNames)
	if err != nil {
		return err
	}

	err = a.runCollector(ctx, reporter, registry.New(env, orgName, registry.WithDetail(a.cfg.RegistryDetail)))
	if err != nil {
		return err
	}

	if a.cfg.Stacks {
		err = a.runCollector(ctx, reporter, stacks.New(env, orgName))
		if err != nil {
			return err
		}
	}

	if a.cfg.AuditTrail {
		err = a.runCollector(ctx, reporter, audit.New(env, orgName))
		if err != nil {
			return err
		}
	}

	return nil
}

// runHistoryOldest resolves the configured run-history age bound into the
// oldest run-creation instant the run walks archive, or the zero time when no
// age bound is configured. It reads the clock once per organization, so every
// workspace in the organization shares one boundary rather than each walk
// sliding it forward as the archive progresses.
func (a *Archiver) runHistoryOldest() time.Time {
	if a.cfg.RunHistoryAge <= 0 {
		return time.Time{}
	}

	return time.Now().Add(-a.cfg.RunHistoryAge)
}

// runCollector runs one uniform [collect.Collector]. It names the reporter's
// phase from the collector and marks it indeterminate: these surfaces have no
// cheap pre-count, so they show a spinner. It returns the collector's error
// unchanged in intent (only a cancellation), wrapped with the collector name
// for context.
func (a *Archiver) runCollector(ctx context.Context, reporter *progress.Reporter, c collect.Collector) error {
	reporter.SetPhase(c.Name())
	reporter.SetTotal(-1)

	err := c.Collect(ctx)
	if err != nil {
		return fmt.Errorf("collect %s: %w", c.Name(), err)
	}

	return nil
}
