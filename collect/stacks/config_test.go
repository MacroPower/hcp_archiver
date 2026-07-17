package stacks_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// writeList answers with a one-page JSON:API list holding the given resource
// documents.
func writeList(t *testing.T, w http.ResponseWriter, items ...string) {
	t.Helper()

	w.Header().Set("Content-Type", "application/vnd.api+json")

	_, err := w.Write([]byte(`{"data": [` + strings.Join(items, ",") + `]}`))
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
	assert.True(t, f.ledger.Collection(runsPrefix).HasUnsettled(),
		"the errored marker holds the runs walk's errored-child gate open")
	assert.True(t, f.ledger.Collection(stacks.ConfigArchivePrefixForTest(st, "proj", "stack")).HasUnsettled(),
		"the errored marker holds the configurations walk's errored-child gate open")

	// A later pass whose listing succeeds settles the marker so the walks can
	// close once every child lands.
	healthy.Store(true)

	require.NoError(t, f.collector.ArchiveRunForTest(t.Context(), "proj", "stack", "sc-1", "sdg-1", run))

	assert.Equal(t, manifest.StatusReferenceCleared, f.status(marker),
		"a successful listing settles the marker")
	assert.False(t, f.ledger.Collection(runsPrefix).HasUnsettled())

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
	assert.True(t, f.ledger.Collection(stacks.ConfigArchivePrefixForTest(st, "proj", "stack")).HasUnsettled(),
		"the errored marker holds the configurations walk's errored-child gate open")

	healthy.Store(true)

	require.NoError(t, f.collector.ArchiveConfigurationForTest(t.Context(), "proj", "stack", cfg))

	assert.Equal(t, manifest.StatusReferenceCleared, f.status(marker),
		"a successful listing settles the marker")
	assert.False(t, f.ledger.Collection(stacks.ConfigArchivePrefixForTest(st, "proj", "stack")).HasUnsettled())
}

func TestArchiveGroupRunsWalkFailurePersists(t *testing.T) {
	t.Parallel()

	// A group hangs off a walk-frozen configuration, so a runs walk that dies
	// on a page fetch must leave a persisted errored marker under the
	// configurations prefix or the group's runs are silently lost for good.
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

	configPrefix := stacks.ConfigArchivePrefixForTest(st, "proj", "stack")
	marker := st.Join(st.StackDeploymentGroupDir("proj", "stack", "sc-1", "sdg-1"),
		"runs-"+stacks.ListingLeafForTest)
	assert.Equal(t, manifest.StatusErrored, f.status(marker),
		"a dropped runs walk records a persisted errored marker")
	assert.True(t, f.ledger.Collection(configPrefix).HasUnsettled(),
		"the errored marker holds the configurations walk's gate open")

	healthy.Store(true)

	require.NoError(t, f.collector.ArchiveGroupForTest(t.Context(), "proj", "stack", "sc-1", group))

	assert.Equal(t, manifest.StatusReferenceCleared, f.status(marker),
		"a settled runs walk settles the marker")
	assert.False(t, f.ledger.Collection(configPrefix).HasUnsettled())
}

func TestArchiveGroupHealsTheLegacyInPrefixMarker(t *testing.T) {
	t.Parallel()

	// An earlier layout kept the runs walk's marker inside the runs prefix it
	// mirrors, where an interrupted run could leave it errored with nothing
	// ever recording that key again: the orphan pinned the runs collection --
	// and the configurations walk above it -- unsettled forever. The group's
	// next visit settles the orphan.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/stack-deployment-groups/sdg-1/stack-deployment-runs",
		func(w http.ResponseWriter, _ *http.Request) {
			writeEmptyList(t, w)
		})

	f := newStacksFixture(t, mux)
	st := f.store

	runsPrefix := stacks.RunArchivePrefixForTest(st, "proj", "stack", "sc-1", "sdg-1")
	legacy := st.Join(runsPrefix, stacks.ListingLeafForTest)
	f.ledger.RecordErrored(legacy, errors.New("interrupted"), true)

	require.True(t, f.ledger.Collection(runsPrefix).HasUnsettled(),
		"the orphaned legacy marker pins the runs collection unsettled")

	group := &tfe.StackDeploymentGroup{ID: "sdg-1", Name: "primary"}
	require.NoError(t, f.collector.ArchiveGroupForTest(t.Context(), "proj", "stack", "sc-1", group))

	assert.Equal(t, manifest.StatusReferenceCleared, f.status(legacy),
		"the orphan settles as a cleared gate")
	assert.False(t, f.ledger.Collection(runsPrefix).HasUnsettled(),
		"the healed collection can settle again")
}

func TestListingFailureAfterSuccessReopensTheMarker(t *testing.T) {
	t.Parallel()

	// The marker reflects the newest enumeration, never the first: a listing
	// that succeeded while the configuration was still mutating and then drops
	// at the frozen boundary must reopen the settled marker, or the walks
	// settle over the gap and the missing children are lost for good.
	var healthy atomic.Bool

	healthy.Store(true)

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

	run := &tfe.StackDeploymentRun{ID: "sdr-1", Status: "succeeded"}

	require.NoError(t, f.collector.ArchiveRunForTest(t.Context(), "proj", "stack", "sc-1", "sdg-1", run))

	marker := st.Join(st.StackRunFile("proj", "stack", "sc-1", "sdg-1", "sdr-1", "steps"), stacks.ListingLeafForTest)
	require.Equal(t, manifest.StatusReferenceCleared, f.status(marker), "the first pass settles the marker")

	healthy.Store(false)

	require.NoError(t, f.collector.ArchiveRunForTest(t.Context(), "proj", "stack", "sc-1", "sdg-1", run))

	assert.Equal(t, manifest.StatusErrored, f.status(marker),
		"a failure after an earlier success reopens the marker")

	runsPrefix := stacks.RunArchivePrefixForTest(st, "proj", "stack", "sc-1", "sdg-1")
	assert.True(t, f.ledger.Collection(runsPrefix).HasUnsettled(),
		"the reopened marker holds the walks open again")
}

func TestInFlightRunKeepsConfigurationsWalkOpen(t *testing.T) {
	t.Parallel()

	// A stack configuration reaches its terminal status when preparation
	// finishes, while its deployment runs keep executing long after. An
	// in-flight run records run.json done (a settled status) and its steps are
	// deferred, so no entry under the configurations prefix is unsettled; only
	// the nested runs walk's withheld settlement carries the openness, and the
	// runs-walk marker must surface it as a pending entry the configurations
	// gate can see, or the configuration freezes over the running deployment
	// and its final state and steps are stranded forever.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/stack-deployment-groups/sdg-1/stack-deployment-runs",
		func(w http.ResponseWriter, _ *http.Request) {
			writeList(t, w, `{"id":"sdr-live","type":"stack-deployment-runs","attributes":{"status":"deploying"}}`)
		})

	f := newStacksFixture(t, mux)
	st := f.store

	group := &tfe.StackDeploymentGroup{ID: "sdg-1", Name: "primary"}

	require.NoError(t, f.collector.ArchiveGroupForTest(t.Context(), "proj", "stack", "sc-1", group))

	marker := st.Join(st.StackDeploymentGroupDir("proj", "stack", "sc-1", "sdg-1"),
		"runs-"+stacks.ListingLeafForTest)
	assert.Equal(t, manifest.StatusPending, f.status(marker),
		"an unsettled nested runs walk leaves the marker pending")

	configPrefix := stacks.ConfigArchivePrefixForTest(st, "proj", "stack")
	assert.True(t, f.ledger.Collection(configPrefix).HasUnsettled(),
		"the pending marker holds the configurations walk open over the in-flight run")
}
