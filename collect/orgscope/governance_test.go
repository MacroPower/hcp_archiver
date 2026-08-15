package orgscope_test

import (
	"testing"

	"github.com/hashicorp/go-tfe/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchivePolicySetVersions(t *testing.T) {
	t.Parallel()

	f := newOrgFixture(t)
	st := f.store

	set := &tfe.PolicySet{
		ID:             "ps-1",
		CurrentVersion: &tfe.PolicySetVersion{ID: "psv-current", Source: "tfe-api"},
		NewestVersion:  &tfe.PolicySetVersion{ID: "psv-newest", Source: "vcs"},
	}

	require.NoError(t, f.collector.ArchivePolicySetVersions(t.Context(), set))

	currentPath := st.PolicySetFile("ps-1", "current-version.json")
	current := f.assertPrimary(t, currentPath, "policy-set-versions", "psv-current")
	assert.Equal(t, "tfe-api", current["source"])

	newestPath := st.PolicySetFile("ps-1", "newest-version.json")
	newest := f.assertPrimary(t, newestPath, "policy-set-versions", "psv-newest")
	assert.Equal(t, "vcs", newest["source"])
}

func TestArchivePolicySetVersionsSameID(t *testing.T) {
	t.Parallel()

	f := newOrgFixture(t)
	st := f.store

	// When the current and newest version are the same, both files are written and
	// carry identical content: the role each names is the information.
	version := &tfe.PolicySetVersion{ID: "psv-1", Source: "tfe-api"}
	set := &tfe.PolicySet{ID: "ps-2", CurrentVersion: version, NewestVersion: version}

	require.NoError(t, f.collector.ArchivePolicySetVersions(t.Context(), set))

	f.assertPrimary(t, st.PolicySetFile("ps-2", "current-version.json"), "policy-set-versions", "psv-1")
	f.assertPrimary(t, st.PolicySetFile("ps-2", "newest-version.json"), "policy-set-versions", "psv-1")
}

func TestArchivePolicySetVersionsNilGuarded(t *testing.T) {
	t.Parallel()

	f := newOrgFixture(t)
	st := f.store

	// A policy set with no hydrated versions writes no version files.
	require.NoError(t, f.collector.ArchivePolicySetVersions(t.Context(), &tfe.PolicySet{ID: "ps-3"}))

	f.assertNoEntry(t, st.PolicySetFile("ps-3", "current-version.json"))
	f.assertNoEntry(t, st.PolicySetFile("ps-3", "newest-version.json"))
}
