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

// archiveTeam archives one team's definition, its member users, and its
// notification configurations.
//
// The team's Users relation is hydrated but renders as bare id refs, so each
// member is archived directly at users/<id>.json (the only capture of who is on
// the team, since go-tfe exposes no user list).
func (c *Collector) archiveTeam(ctx context.Context, team *tfe.Team) error {
	err := c.mutableValue(ctx, c.env.Store().TeamFile(team.ID, "team.json"), team)
	if err != nil {
		return err
	}

	for _, u := range team.Users {
		uErr := c.archiveUser(ctx, u)
		if uErr != nil {
			return uErr
		}
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
				SubscribableChoice: &tfe.NotificationConfigurationSubscribableChoice{
					Team: &tfe.Team{ID: teamID},
				},
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list notification configs: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
}

// collectMemberships archives the organization roster and, from each
// membership's hydrated user relation, the referenced users.
//
// Unlike the other bulk lists this paginates the items itself so the individual
// memberships are in hand: memberships.json is written from them, then each
// membership's User is archived at users/<id>.json (the only capture of who is in
// the org, since go-tfe exposes no user list). A read that does not complete is
// logged and skipped, leaving the prior roster untouched rather than overwriting
// it with an empty list.
func (c *Collector) collectMemberships(ctx context.Context) error {
	memberships, ok, err := paginate(ctx, c, "memberships",
		func(
			ctx context.Context,
			tc *tfe.Client,
			o tfe.ListOptions,
		) ([]*tfe.OrganizationMembership, *tfe.Pagination, error) {
			l, e := tc.OrganizationMemberships.List(ctx, c.org, &tfe.OrganizationMembershipListOptions{
				ListOptions: o,
				Include: []tfe.OrgMembershipIncludeOpt{
					tfe.OrgMembershipUser,
				},
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list memberships: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
	if err != nil {
		return err
	}

	// A skipped read leaves the roster for the next run; a successful read, even
	// an empty one, is recorded so it settles rather than re-fetching forever.
	if !ok {
		return nil
	}

	err = c.mutableValue(ctx, c.env.Store().Memberships(), memberships)
	if err != nil {
		return err
	}

	// Roster members not on any team are archived here rather than by the
	// concurrent team pass, so fan them across the concurrency budget the same way
	// enumerate does; archiveUser claims each user and is safe to run in parallel.
	return fanOut(ctx, c, memberships, func(ctx context.Context, m *tfe.OrganizationMembership) error {
		return c.archiveUser(ctx, m.User)
	})
}
