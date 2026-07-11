package stacks

import (
	"context"
	"fmt"
	"io"

	tfe "github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// collectConfigurations walks a stack's configurations newest-first, archiving
// each snapshot with its JSON schemas, diagnostics, and deployment groups, and
// halting once it reaches an already-archived terminal configuration.
func (c *Collector) collectConfigurations(ctx context.Context, project string, stack *tfe.Stack) error {
	pager := newPager(
		c.env,
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.StackConfiguration, *tfe.Pagination, error) {
			l, e := tc.StackConfigurations.List(ctx, stack.ID, &tfe.StackConfigurationListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list stack configurations: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)

	describe := func(cfg *tfe.StackConfiguration) collect.Item {
		return collect.Item{
			RelPath:   c.env.Store().StackConfigurationFile(project, stack.Name, cfg.ID, "configuration.json"),
			CreatedAt: cfg.CreatedAt,
			Terminal:  configTerminal(cfg.Status),
			Archive: func(ctx context.Context) error {
				return c.archiveConfiguration(ctx, project, stack.Name, cfg)
			},
		}
	}

	// The cursor key is a synthetic stack id, but the entries live under the
	// stack's configurations directory; pass that real prefix so the errored-child
	// gate scans the stack shard rather than the org-root shard the id routes to.
	archivePrefix := configArchivePrefix(c.env.Store(), project, stack.Name)

	err := collect.Walk(ctx, c.env, configKey(stack.ID), pager, describe,
		collect.WithArchivePrefix(archivePrefix))
	if err != nil {
		return fmt.Errorf("walk stack configurations: %w", err)
	}

	return nil
}

// archiveConfiguration archives one configuration snapshot: its metadata, the
// provider JSON schemas, its diagnostics, and the deployment groups beneath it.
// Only a context cancellation propagates.
func (c *Collector) archiveConfiguration(
	ctx context.Context,
	project, stackName string,
	cfg *tfe.StackConfiguration,
) error {
	st := c.env.Store()

	cfgFile := st.StackConfigurationFile(project, stackName, cfg.ID, "configuration.json")

	err := c.env.Mutable(ctx, cfgFile, func(_ context.Context) (any, error) {
		return cfg, nil
	})
	if err != nil {
		return wrap(err)
	}

	// The provider JSON schemas are immutable but exist only once the
	// configuration finishes preparing; fetch them only when it is terminal, so a
	// still-preparing configuration does not record a premature settled gap for
	// schemas it has yet to produce. Diagnostics stay mutable below, so a prep
	// error is still captured while the configuration is in flight.
	if configTerminal(cfg.Status) {
		schemaFile := st.StackConfigurationFile(project, stackName, cfg.ID, "json-schemas.json")

		err = c.env.Bytes(ctx, schemaFile, func(ctx context.Context) ([]byte, error) {
			var raw []byte

			e := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
				var d error

				raw, d = tc.StackConfigurations.JSONSchemas(ctx, cfg.ID)
				if d != nil {
					return fmt.Errorf("read stack configuration json schemas: %w", d)
				}

				return nil
			})

			return raw, wrap(e)
		})
		if err != nil {
			return wrap(err)
		}
	}

	diagFile := st.StackConfigurationFile(project, stackName, cfg.ID, "diagnostics.json")

	err = c.env.Mutable(ctx, diagFile, func(ctx context.Context) (any, error) {
		return c.configDiagnostics(ctx, cfg.ID)
	})
	if err != nil {
		return wrap(err)
	}

	return c.tolerate(ctx, c.collectGroups(ctx, project, stackName, cfg.ID))
}

// configDiagnostics reads a configuration's diagnostics as an id-serializable
// slice, the payload archived to its diagnostics file.
func (c *Collector) configDiagnostics(ctx context.Context, configID string) (any, error) {
	var list *tfe.StackDiagnosticsList

	err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		var e error

		list, e = tc.StackConfigurations.Diagnostics(ctx, configID)
		if e != nil {
			return fmt.Errorf("read stack configuration diagnostics: %w", e)
		}

		return nil
	})
	if err != nil {
		return nil, wrap(err)
	}

	return list.Items, nil
}

// collectGroups archives every deployment group under a configuration, then the
// runs that hang off each group. A per-group failure is logged and skipped.
func (c *Collector) collectGroups(ctx context.Context, project, stackName, configID string) error {
	groups, err := tfeclient.Paginate(
		ctx,
		c.env.Client(),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.StackDeploymentGroup, *tfe.Pagination, error) {
			l, e := tc.StackDeploymentGroups.List(ctx, configID, &tfe.StackDeploymentGroupListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list stack deployment groups: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
	if err != nil {
		return wrap(err)
	}

	for _, group := range groups {
		err = c.tolerate(ctx, c.archiveGroup(ctx, project, stackName, configID, group))
		if err != nil {
			return err
		}
	}

	return nil
}

// archiveGroup archives one deployment group's metadata and walks its runs. Only
// a context cancellation propagates.
func (c *Collector) archiveGroup(
	ctx context.Context,
	project, stackName, configID string,
	group *tfe.StackDeploymentGroup,
) error {
	groupFile := c.env.Store().StackDeploymentGroupFile(project, stackName, configID, group.ID, "group.json")

	err := c.env.Mutable(ctx, groupFile, func(_ context.Context) (any, error) {
		return group, nil
	})
	if err != nil {
		return wrap(err)
	}

	return c.tolerate(ctx, c.collectRuns(ctx, project, stackName, configID, group.ID))
}

// collectRuns walks a deployment group's runs newest-first, archiving each run
// and its steps and halting at already-archived terminal history.
func (c *Collector) collectRuns(ctx context.Context, project, stackName, configID, groupID string) error {
	pager := newPager(
		c.env,
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.StackDeploymentRun, *tfe.Pagination, error) {
			l, e := tc.StackDeploymentRuns.List(ctx, groupID, &tfe.StackDeploymentRunListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list stack deployment runs: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)

	describe := func(run *tfe.StackDeploymentRun) collect.Item {
		return collect.Item{
			RelPath:   c.env.Store().StackRunFile(project, stackName, configID, groupID, run.ID, "run.json"),
			CreatedAt: run.CreatedAt,
			Terminal:  runTerminal(run.Status),
			Archive: func(ctx context.Context) error {
				return c.archiveRun(ctx, project, stackName, configID, groupID, run)
			},
		}
	}

	// The cursor key is a synthetic group id; the entries live under the group's
	// runs directory. Pass that prefix so the errored-child gate reaches a step
	// artifact left errored below a done run boundary.
	archivePrefix := runArchivePrefix(c.env.Store(), project, stackName, configID, groupID)

	err := collect.Walk(ctx, c.env, runKey(groupID), pager, describe,
		collect.WithArchivePrefix(archivePrefix))
	if err != nil {
		return fmt.Errorf("walk stack deployment runs: %w", err)
	}

	return nil
}

// archiveRun archives one deployment run's mutable metadata and, once the run is
// terminal, its per-step artifacts. Only a context cancellation propagates.
func (c *Collector) archiveRun(
	ctx context.Context,
	project, stackName, configID, groupID string,
	run *tfe.StackDeploymentRun,
) error {
	runFile := c.env.Store().StackRunFile(project, stackName, configID, groupID, run.ID, "run.json")

	err := c.env.Mutable(ctx, runFile, func(_ context.Context) (any, error) {
		return run, nil
	})
	if err != nil {
		return wrap(err)
	}

	// A run's steps carry immutable per-step artifacts (the plan and apply
	// descriptions). Fetch them only once the run is terminal, so an in-flight run
	// does not record a premature settled gap for an artifact it has yet to
	// produce, mirroring the workspace run collector.
	if !runTerminal(run.Status) {
		return nil
	}

	return c.tolerate(ctx, c.collectSteps(ctx, project, stackName, configID, groupID, run.ID))
}

// collectSteps archives every step of a deployment run. A per-step failure is
// logged and skipped.
func (c *Collector) collectSteps(ctx context.Context, project, stackName, configID, groupID, runID string) error {
	steps, err := tfeclient.Paginate(
		ctx,
		c.env.Client(),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.StackDeploymentStep, *tfe.Pagination, error) {
			l, e := tc.StackDeploymentSteps.List(ctx, runID, &tfe.StackDeploymentStepsListOptions{ListOptions: o})
			if e != nil {
				return nil, nil, fmt.Errorf("list stack deployment steps: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
	if err != nil {
		return wrap(err)
	}

	for _, step := range steps {
		err = c.tolerate(ctx, c.archiveStep(ctx, project, stackName, configID, groupID, runID, step))
		if err != nil {
			return err
		}
	}

	return nil
}

// archiveStep archives one step: its metadata, its plan and apply description
// artifacts, and its diagnostics. Only a context cancellation propagates.
//
//nolint:gocritic // The step path is genuinely this deep; each id names a level.
func (c *Collector) archiveStep(
	ctx context.Context,
	project, stackName, configID, groupID, runID string,
	step *tfe.StackDeploymentStep,
) error {
	st := c.env.Store()

	stepFile := st.StackStepFile(project, stackName, configID, groupID, runID, step.ID, "step.json")

	err := c.env.Mutable(ctx, stepFile, func(_ context.Context) (any, error) {
		return step, nil
	})
	if err != nil {
		return wrap(err)
	}

	artifacts := map[string]tfe.StackDeploymentStepArtifactType{
		"plan-description.json":  tfe.StackDeploymentStepArtifactPlanDescription,
		"apply-description.json": tfe.StackDeploymentStepArtifactApplyDescription,
	}

	for file, kind := range artifacts {
		artFile := st.StackStepFile(project, stackName, configID, groupID, runID, step.ID, file)

		err = c.env.Blob(ctx, artFile, func(ctx context.Context) (io.Reader, error) {
			return c.stepArtifact(ctx, step.ID, kind)
		})
		if err != nil {
			return wrap(err)
		}
	}

	diagFile := st.StackStepFile(project, stackName, configID, groupID, runID, step.ID, "diagnostics.json")

	return wrap(c.env.Mutable(ctx, diagFile, func(ctx context.Context) (any, error) {
		return c.stepDiagnostics(ctx, step.ID)
	}))
}

// stepArtifact opens a step's named description artifact for streaming to disk.
func (c *Collector) stepArtifact(
	ctx context.Context,
	stepID string,
	kind tfe.StackDeploymentStepArtifactType,
) (io.Reader, error) {
	var r io.ReadCloser

	err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		var e error

		r, e = tc.StackDeploymentSteps.Artifacts(ctx, stepID, kind)
		if e != nil {
			return fmt.Errorf("read stack deployment step artifact: %w", e)
		}

		return nil
	})
	if err != nil {
		return nil, wrap(err)
	}

	return r, nil
}

// stepDiagnostics reads a step's diagnostics as an id-serializable slice, the
// payload archived to its diagnostics file.
func (c *Collector) stepDiagnostics(ctx context.Context, stepID string) (any, error) {
	var list *tfe.StackDiagnosticsList

	err := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		var e error

		list, e = tc.StackDeploymentSteps.Diagnostics(ctx, stepID)
		if e != nil {
			return fmt.Errorf("read stack deployment step diagnostics: %w", e)
		}

		return nil
	})
	if err != nil {
		return nil, wrap(err)
	}

	return list.Items, nil
}
