package orgscope_test

import (
	"testing"

	"github.com/hashicorp/go-tfe/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveOAuthClient(t *testing.T) {
	t.Parallel()

	f := newOrgFixture(t)
	st := f.store

	client := &tfe.OAuthClient{
		ID: "oc-1",
		OAuthTokens: []*tfe.OAuthToken{
			{ID: "ot-1", UID: "octo", ServiceProviderUser: "octocat", HasSSHKey: true},
			{ID: "ot-2", UID: "hubot"},
		},
	}

	require.NoError(t, f.collector.ArchiveOAuthClient(t.Context(), client))

	f.assertPrimary(t, st.OAuthClientFile("oc-1", "oauth-client.json"), "oauth-clients", "oc-1")

	tok1 := f.assertPrimary(t, st.OAuthTokenFile("oc-1", "ot-1"), "oauth-tokens", "ot-1")
	assert.Equal(t, "octo", tok1["uid"])
	assert.Equal(t, "octocat", tok1["service-provider-user"])
	assert.Equal(t, true, tok1["has-ssh-key"])

	tok2 := f.assertPrimary(t, st.OAuthTokenFile("oc-1", "ot-2"), "oauth-tokens", "ot-2")
	assert.Equal(t, "hubot", tok2["uid"])
}
