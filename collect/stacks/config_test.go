package stacks_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tfe "github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/collect/stacks"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// stacksFixture builds a stacks collector over a client pointed at a fake
// server, with a real store and ledger, so the walk-frozen child-listing paths
// can be exercised end to end.
type stacksFixture struct {
	collector *stacks.Collector
	store     *store.Store
	ledger    *manifest.Ledger
}

// newStacksFixture serves mux (with a go-tfe construction ping added) and
// builds a collector over a client pointed at it. Server-error retries are
// disabled so a handler answering 500 fails the listing immediately.
func newStacksFixture(t *testing.T, mux *http.ServeMux) stacksFixture {
	t.Helper()

	mux.HandleFunc("/api/v2/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := tfeclient.New(
		tfeclient.WithToken("test-token"),
		tfeclient.WithAddress(srv.URL),
		tfeclient.WithServerErrorRetry(0, 0),
	)
	require.NoError(t, err)

	st := store.New(t.TempDir())

	ledger, err := manifest.Load(st.Root())
	require.NoError(t, err)

	ledger.StartRun()

	env := collect.NewEnv(client, st, ledger, collect.WithAbsentConfirm(0))

	return stacksFixture{
		collector: stacks.New(env, "example-org"),
		store:     st,
		ledger:    ledger,
	}
}

// status returns the recorded ledger status for relPath, or the empty string
// when no entry exists.
func (f stacksFixture) status(relPath string) manifest.Status {
	entry, ok := f.ledger.Entry(relPath)
	if !ok {
		return ""
	}

	return entry.Status
}

// writeEmptyList answers with an empty JSON:API list document.
func writeEmptyList(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	w.Header().Set("Content-Type", "application/vnd.api+json")

	_, err := w.Write([]byte(`{"data": []}`))
	require.NoError(t, err)
}

func TestArchiveRunStepsListingFailurePersists(t *testing.T) {
	t.Parallel()

	// A terminal run is frozen by the runs walk once its run.json is done, so a
	// dropped steps listing must leave a persisted errored marker: without it
	// the walk settles over the gap and never revisits the run, and its steps
	// are silently lost for good.
	var healthy atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/stack-deployment-runs/sdr-1/stack-deployment-steps",
		func(w http.ResponseWriter, _ *http.Request) {
			if !healthy.Load() {
				w.WriteHeader(http.StatusInternalServerError)

				return
			}

			writeEmptyList(t, w)
		})

	f := newStacksFixture(t, mux)
	st := f.store

	run := &tfe.StackDeploymentRun{ID: "sdr-1", Status: tfe.DeploymentRunStatusSucceeded}

	require.NoError(t, f.collector.ArchiveRunForTest(t.Context(), "proj", "stack", "sc-1", "sdg-1", run))

	marker := st.Join(st.StackRunFile("proj", "stack", "sc-1", "sdg-1", "sdr-1", "steps"), stacks.ListingLeafForTest)
	assert.Equal(t, manifest.StatusErrored, f.status(marker),
		"a dropped steps listing records a persisted errored marker")

	runsPrefix := stacks.RunArchivePrefixForTest(st, "proj", "stack", "sc-1", "sdg-1")
	assert.True(t, f.ledger.HasUnsettledUnder(runsPrefix),
		"the errored marker holds the runs walk's errored-child gate open")
	assert.True(t, f.ledger.HasUnsettledUnder(stacks.ConfigArchivePrefixForTest(st, "proj", "stack")),
		"the errored marker holds the configurations walk's errored-child gate open")

	// A later pass whose listing succeeds settles the marker so the walks can
	// close once every child lands.
	healthy.Store(true)

	require.NoError(t, f.collector.ArchiveRunForTest(t.Context(), "proj", "stack", "sc-1", "sdg-1", run))

	assert.Equal(t, manifest.StatusNotApplicable, f.status(marker),
		"a successful listing settles the marker")
	assert.False(t, f.ledger.HasUnsettledUnder(runsPrefix))

	exists, err := st.Exists(marker)
	require.NoError(t, err)
	assert.False(t, exists, "the marker is a ledger entry only, never a file")
}

func TestArchiveConfigurationGroupsListingFailurePersists(t *testing.T) {
	t.Parallel()

	// A configuration is frozen by the configurations walk once terminal, so a
	// dropped deployment-groups listing must leave a persisted errored marker
	// or the whole subtree beneath the configuration is silently lost for good.
	var healthy atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/stack-configurations/sc-1/stack-diagnostics",
		func(w http.ResponseWriter, _ *http.Request) {
			writeEmptyList(t, w)
		})
	mux.HandleFunc("/api/v2/stack-configurations/sc-1/stack-deployment-groups",
		func(w http.ResponseWriter, _ *http.Request) {
			if !healthy.Load() {
				w.WriteHeader(http.StatusInternalServerError)

				return
			}

			writeEmptyList(t, w)
		})

	f := newStacksFixture(t, mux)
	st := f.store

	// A non-terminal status skips the JSON-schemas fetch, isolating the test on
	// the groups enumeration; the marker is recorded either way.
	cfg := &tfe.StackConfiguration{ID: "sc-1", Status: tfe.StackConfigurationStatusPending}

	require.NoError(t, f.collector.ArchiveConfigurationForTest(t.Context(), "proj", "stack", cfg))

	groupsDir := st.StackConfigurationFile("proj", "stack", "sc-1", "deployment-groups")
	marker := st.Join(groupsDir, stacks.ListingLeafForTest)
	assert.Equal(t, manifest.StatusErrored, f.status(marker),
		"a dropped groups listing records a persisted errored marker")
	assert.True(t, f.ledger.HasUnsettledUnder(stacks.ConfigArchivePrefixForTest(st, "proj", "stack")),
		"the errored marker holds the configurations walk's errored-child gate open")

	healthy.Store(true)

	require.NoError(t, f.collector.ArchiveConfigurationForTest(t.Context(), "proj", "stack", cfg))

	assert.Equal(t, manifest.StatusNotApplicable, f.status(marker),
		"a successful listing settles the marker")
	assert.False(t, f.ledger.HasUnsettledUnder(stacks.ConfigArchivePrefixForTest(st, "proj", "stack")))
}

func TestArchiveGroupRunsWalkFailurePersists(t *testing.T) {
	t.Parallel()

	// A group hangs off a walk-frozen configuration, so a runs walk that dies
	// on a page fetch must leave a persisted errored marker under the group's
	// runs prefix or the group's runs are silently lost for good.
	var healthy atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/stack-deployment-groups/sdg-1/stack-deployment-runs",
		func(w http.ResponseWriter, _ *http.Request) {
			if !healthy.Load() {
				w.WriteHeader(http.StatusInternalServerError)

				return
			}

			writeEmptyList(t, w)
		})

	f := newStacksFixture(t, mux)
	st := f.store

	group := &tfe.StackDeploymentGroup{ID: "sdg-1", Name: "primary"}

	require.NoError(t, f.collector.ArchiveGroupForTest(t.Context(), "proj", "stack", "sc-1", group))

	runsPrefix := stacks.RunArchivePrefixForTest(st, "proj", "stack", "sc-1", "sdg-1")
	marker := st.Join(runsPrefix, stacks.ListingLeafForTest)
	assert.Equal(t, manifest.StatusErrored, f.status(marker),
		"a dropped runs walk records a persisted errored marker")
	assert.True(t, f.ledger.HasUnsettledUnder(runsPrefix),
		"the errored marker holds the errored-child gates open")

	healthy.Store(true)

	require.NoError(t, f.collector.ArchiveGroupForTest(t.Context(), "proj", "stack", "sc-1", group))

	assert.Equal(t, manifest.StatusNotApplicable, f.status(marker),
		"a completed runs walk settles the marker")
	assert.False(t, f.ledger.HasUnsettledUnder(runsPrefix))
}
