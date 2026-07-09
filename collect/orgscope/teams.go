package orgscope

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-tfe"
)

// collectOrganization archives the organization record itself.
func (c *Collector) collectOrganization(ctx context.Context) error {
	err := c.env.Mutable(ctx, c.env.Store().Org(), func(ctx context.Context) (any, error) {
		var org *tfe.Organization

		derr := c.env.Client().Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
			var e error

			org, e = tc.Organizations.Read(ctx, c.org)
			if e != nil {
				return fmt.Errorf("read organization: %w", e)
			}

			return nil
		})
		if derr != nil {
			return nil, fmt.Errorf("organization: %w", derr)
		}

		return org, nil
	})
	if err != nil {
		return fmt.Errorf("archive organization: %w", err)
	}

	return nil
}

// collectTeams archives each team with its organization-access matrix, members,
// and SSO/SCIM linkage, plus that team's notification configurations.
func (c *Collector) collectTeams(ctx context.Context) error {
	return enumerate(ctx, c, "teams",
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.Team, *tfe.Pagination, error) {
			l, e := tc.Teams.List(ctx, c.org, &tfe.TeamListOptions{
				ListOptions: o,
				Include: []tfe.TeamIncludeOpt{
					tfe.TeamUsers,
					tfe.TeamOrganizationMemberships,
				},
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list teams: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
		c.archiveTeam,
	)
}

// archiveTeam archives one team's definition and its notification
// configurations.
func (c *Collector) archiveTeam(ctx context.Context, team *tfe.Team) error {
	err := c.mutableValue(ctx, c.env.Store().TeamFile(team.ID, "team.json"), team)
	if err != nil {
		return err
	}

	return c.collectTeamNotifications(ctx, team.ID)
}

// collectTeamNotifications archives the notification configurations subscribed
// to the team with the given id.
func (c *Collector) collectTeamNotifications(ctx context.Context, teamID string) error {
	relPath := c.env.Store().TeamFile(teamID, "notification-configs.json")

	return archiveList(ctx, c, relPath, "team notification configs",
		func(
			ctx context.Context,
			tc *tfe.Client,
			o tfe.ListOptions,
		) ([]*tfe.NotificationConfiguration, *tfe.Pagination, error) {
			l, e := tc.NotificationConfigurations.List(ctx, teamID, &tfe.NotificationConfigurationListOptions{
				ListOptions: o,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list notification configs: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
}

// collectMemberships archives the organization roster with each membership's
// user and team references.
func (c *Collector) collectMemberships(ctx context.Context) error {
	return archiveList(ctx, c, c.env.Store().Memberships(), "memberships",
		func(
			ctx context.Context,
			tc *tfe.Client,
			o tfe.ListOptions,
		) ([]*tfe.OrganizationMembership, *tfe.Pagination, error) {
			l, e := tc.OrganizationMemberships.List(ctx, c.org, &tfe.OrganizationMembershipListOptions{
				ListOptions: o,
				Include: []tfe.OrgMembershipIncludeOpt{
					tfe.OrgMembershipUser,
					tfe.OrgMembershipTeam,
				},
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list memberships: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
}
