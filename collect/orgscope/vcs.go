package orgscope

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-tfe"
)

// collectOAuthClients archives each VCS OAuth client with its tokens and
// projects. The client secret is redacted by the serializer.
func (c *Collector) collectOAuthClients(ctx context.Context) error {
	return enumerate(ctx, c, "oauth clients",
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.OAuthClient, *tfe.Pagination, error) {
			l, e := tc.OAuthClients.List(ctx, c.org, &tfe.OAuthClientListOptions{
				ListOptions: o,
				Include: []tfe.OAuthClientIncludeOpt{
					tfe.OauthClientOauthTokens,
					tfe.OauthClientProjects,
				},
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list oauth clients: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
		func(ctx context.Context, client *tfe.OAuthClient) error {
			return c.mutableValue(ctx, c.env.Store().OAuthClient(client.ID), client)
		},
	)
}

// collectGitHubAppInstallations archives the GitHub App VCS installations
// visible to the archiving identity as metadata only.
func (c *Collector) collectGitHubAppInstallations(ctx context.Context) error {
	return archiveList(ctx, c, c.env.Store().GitHubAppInstallations(), "github app installations",
		func(
			ctx context.Context,
			tc *tfe.Client,
			o tfe.ListOptions,
		) ([]*tfe.GHAInstallation, *tfe.Pagination, error) {
			l, e := tc.GHAInstallations.List(ctx, &tfe.GHAInstallationListOptions{
				ListOptions: o,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list github app installations: %w", e)
			}

			return l.Items, l.Pagination, nil
		},
	)
}
