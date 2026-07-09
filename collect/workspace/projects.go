package workspace

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-tfe"
)

// CollectProject archives one project: its record, its project-scoped
// notification configurations, and its team access. It is called once per
// project before the project's workspaces are archived.
//
// It returns only on a context cancellation; a single missing or failed object
// is recorded in the ledger and does not abort the collector.
func (c *Collector) CollectProject(ctx context.Context, p *tfe.Project) error {
	st := c.env.Store()
	name := p.Name

	c.env.SetTarget(name)

	projectID := p.ID

	err := mutableOne(ctx, c, st.ProjectFile(name, "project.json"),
		func(ctx context.Context, tc *tfe.Client) (*tfe.Project, error) {
			return tc.Projects.ReadWithOptions(ctx, projectID, tfe.ProjectReadOptions{
				Include: []tfe.ProjectIncludeOpt{tfe.ProjectEffectiveTagBindings},
			})
		})
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
