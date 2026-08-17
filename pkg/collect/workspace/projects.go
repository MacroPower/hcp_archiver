package workspace

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hashicorp/go-tfe"
)

// CollectProject archives one project: its record, its project-scoped
// notification configurations, and its team access. It is called once per
// project before the project's workspaces are archived. Projects archive
// concurrently, so it does not touch the progress target; the phase bar tracks
// them instead.
//
// It returns only on a context cancellation; a single missing or failed object
// is recorded in the ledger and does not abort the collector. A project whose
// name-keyed directory is owned by a different project (a reused name) is
// skipped with its surface dropped, so the deleted project's metadata is never
// overwritten; the workspaces nested beneath it stay individually protected by
// their own directory claims.
func (c *Collector) CollectProject(ctx context.Context, p *tfe.Project) error {
	st := c.env.Store()
	name := p.Name
	projectID := p.ID

	renamedFrom, err := c.env.ClaimDir(st.ProjectDir(name), projectID)
	if err != nil {
		slog.ErrorContext(ctx, "project directory not claimed; skipping",
			slog.String("project", name),
			slog.Any("error", err),
		)

		return nil
	}

	if renamedFrom != "" {
		slog.WarnContext(ctx, "project was renamed; its prior archive is kept",
			slog.String("project", name),
			slog.String("previous_name", renamedFrom),
		)
	}

	err = c.archiveProject(ctx, name, projectID)
	if err != nil {
		return err
	}

	err = listMutable(
		ctx,
		c,
		st.ProjectFile(name, "notification-configs.json"),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.NotificationConfiguration, *tfe.Pagination, error) {
			l, e := tc.NotificationConfigurations.List(ctx, projectID, &tfe.NotificationConfigurationListOptions{
				ListOptions: o,
				SubscribableChoice: &tfe.NotificationConfigurationSubscribableChoice{
					Project: &tfe.Project{ID: projectID},
				},
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list notification configs: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
	if err != nil {
		return err
	}

	return listMutable(
		ctx,
		c,
		st.ProjectFile(name, "team-access.json"),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.TeamProjectAccess, *tfe.Pagination, error) {
			l, e := tc.TeamProjectAccess.List(ctx, tfe.TeamProjectAccessListOptions{
				ListOptions: o,
				ProjectID:   projectID,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list team project access: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
}

// archiveProject reads the project once with its effective tag bindings and
// splits it into project.json and effective-tag-bindings.json.
//
// The include hydrates the effective tag bindings, but project.json renders them
// as bare id refs, so they are archived directly as their own file. Both are
// mutable and re-read every run, so one read feeds both without a gating hazard;
// a read failure records project.json errored and leaves the bindings unwritten,
// since they share the same read.
func (c *Collector) archiveProject(ctx context.Context, name, projectID string) error {
	st := c.env.Store()
	projectPath := st.ProjectFile(name, "project.json")

	var project *tfe.Project

	err := wrapArchive(projectPath, c.env.Mutable(ctx, projectPath, func(ctx context.Context) (any, error) {
		p, readErr := doRead(ctx, c, projectPath, func(ctx context.Context, tc *tfe.Client) (*tfe.Project, error) {
			return tc.Projects.ReadWithOptions(ctx, projectID, tfe.ProjectReadOptions{
				Include: []tfe.ProjectIncludeOpt{tfe.ProjectEffectiveTagBindings},
			})
		})
		if readErr != nil {
			return nil, readErr
		}

		project = p

		return p, nil
	}))
	if err != nil {
		return err
	}

	if project == nil {
		return nil
	}

	return c.mutable(ctx, st.ProjectFile(name, "effective-tag-bindings.json"), project.EffectiveTagBindings)
}
