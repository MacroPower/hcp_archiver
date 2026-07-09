package archiver

import (
	"context"
	"fmt"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/collect/audit"
	"go.jacobcolvin.com/hcp_archiver/collect/orgscope"
	"go.jacobcolvin.com/hcp_archiver/collect/registry"
	"go.jacobcolvin.com/hcp_archiver/collect/stacks"
	"go.jacobcolvin.com/hcp_archiver/collect/workspace"
)

// collectOrg runs the domain collectors against one organization in order.
//
// Each collector is best-effort: a single missing or failed object is recorded
// and the walk continues, so a collector returns non-nil only on a cancellation
// of ctx, which stops the whole organization. Optional surfaces run only when
// their configuration toggle is set.
func (a *Archiver) collectOrg(ctx context.Context, env *collect.Env, orgName string) error {
	err := a.runCollector(ctx, orgscope.New(env, orgName, orgscope.WithHYOK(a.cfg.HYOK)))
	if err != nil {
		return err
	}

	wsc := workspace.New(env, orgName)

	projectNames, err := a.collectProjects(ctx, env, orgName, wsc)
	if err != nil {
		return err
	}

	err = a.collectWorkspaces(ctx, env, orgName, wsc, projectNames)
	if err != nil {
		return err
	}

	err = a.runCollector(ctx, registry.New(env, orgName, registry.WithDetail(a.cfg.RegistryDetail)))
	if err != nil {
		return err
	}

	if a.cfg.Stacks {
		err = a.runCollector(ctx, stacks.New(env, orgName))
		if err != nil {
			return err
		}
	}

	if a.cfg.AuditTrail {
		err = a.runCollector(ctx, audit.New(env, orgName))
		if err != nil {
			return err
		}
	}

	return nil
}

// runCollector runs one uniform [collect.Collector]. It returns the collector's
// error unchanged in intent (only a cancellation), wrapped with the collector
// name for context.
func (a *Archiver) runCollector(ctx context.Context, c collect.Collector) error {
	err := c.Collect(ctx)
	if err != nil {
		return fmt.Errorf("collect %s: %w", c.Name(), err)
	}

	return nil
}
