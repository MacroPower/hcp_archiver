package orgscope

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-tfe"
)

// collectOAuthClients archives each VCS OAuth client with its tokens and
// projects, stored exactly as the API returns them.
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
		c.archiveOAuthClient,
	)
}

// archiveOAuthClient archives one VCS OAuth client and its hydrated tokens.
//
// The parent renders its token relation as bare id refs, so each token is
// archived directly as its own primary object; tokens carry only metadata
// (uid, created-at, has-ssh-key, service-provider-user) and no secret.
func (c *Collector) archiveOAuthClient(ctx context.Context, client *tfe.OAuthClient) error {
	st := c.env.Store()

	err := c.mutableValue(ctx, st.OAuthClientFile(client.ID, "oauth-client.json"), client)
	if err != nil {
		return err
	}

	for _, tok := range client.OAuthTokens {
		err = c.mutableValue(ctx, st.OAuthTokenFile(client.ID, tok.ID), tok)
		if err != nil {
			return err
		}
	}

	return nil
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
