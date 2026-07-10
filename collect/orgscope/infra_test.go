package orgscope_test

import (
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveHYOKConfiguration(t *testing.T) {
	t.Parallel()

	f := newOrgFixture(t)
	st := f.store

	config := &tfe.HYOKConfiguration{
		ID:     "hyok-1",
		Name:   "prod-key",
		KEKID:  "kek-abc",
		Status: "available",
		OIDCConfiguration: &tfe.OIDCConfigurationTypeChoice{
			GCPOIDCConfiguration: &tfe.GCPOIDCConfiguration{
				ID:                  "gcp-oidc-1",
				ServiceAccountEmail: "sa@example.iam.gserviceaccount.com",
			},
		},
		KeyVersions: []*tfe.HYOKCustomerKeyVersion{
			{ID: "kv-1", KeyVersion: "1", Status: "available"},
			{ID: "kv-2", KeyVersion: "2", Status: "available"},
		},
	}

	require.NoError(t, f.collector.ArchiveHYOKConfiguration(t.Context(), config))

	// The parent keeps its own attributes.
	attrs := f.assertPrimary(t,
		st.HYOKConfigurationFile("hyok-1", "hyok-configuration.json"),
		"hyok-configurations", "hyok-1")
	assert.Equal(t, "prod-key", attrs["name"])
	assert.Equal(t, "kek-abc", attrs["kek-id"])

	// The polymorphic OIDC choice is unwrapped to the one concrete configuration
	// and archived with its attributes, not a bare id ref.
	oidc := f.assertPrimary(t,
		st.HYOKConfigurationFile("hyok-1", "oidc-configuration.json"),
		"gcp-oidc-configurations", "gcp-oidc-1")
	assert.Equal(t, "sa@example.iam.gserviceaccount.com", oidc["service-account-email"])

	// Each hydrated key version becomes its own file.
	kv1 := f.assertPrimary(t, st.HYOKKeyVersionFile("hyok-1", "kv-1"), "hyok-customer-key-versions", "kv-1")
	assert.Equal(t, "1", kv1["key-version"])

	kv2 := f.assertPrimary(t, st.HYOKKeyVersionFile("hyok-1", "kv-2"), "hyok-customer-key-versions", "kv-2")
	assert.Equal(t, "2", kv2["key-version"])
}

func TestArchiveHYOKConfigurationSkipsAllNilOIDC(t *testing.T) {
	t.Parallel()

	f := newOrgFixture(t)
	st := f.store

	// A transiently non-hydrated OIDC choice (all concrete pointers nil) records
	// no file and no ledger entry, so HYOK's Mutable re-enumeration retries next
	// run rather than settling a permanent gap.
	config := &tfe.HYOKConfiguration{
		ID:                "hyok-2",
		Name:              "staging-key",
		OIDCConfiguration: &tfe.OIDCConfigurationTypeChoice{},
	}

	require.NoError(t, f.collector.ArchiveHYOKConfiguration(t.Context(), config))

	f.assertPrimary(t,
		st.HYOKConfigurationFile("hyok-2", "hyok-configuration.json"),
		"hyok-configurations", "hyok-2")
	f.assertNoEntry(t, st.HYOKConfigurationFile("hyok-2", "oidc-configuration.json"))
}
