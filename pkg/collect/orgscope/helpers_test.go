package orgscope_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
	"go.jacobcolvin.com/hcp_archiver/pkg/collect/orgscope"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
	"go.jacobcolvin.com/hcp_archiver/pkg/tfeclient"
)

// orgFixture builds an org-scoped collector over a temp-dir store and ledger, so
// the value-in-hand archive paths can be exercised without a network.
type orgFixture struct {
	collector *orgscope.Collector
	store     *store.Store
	ledger    *manifest.Ledger
}

func newOrgFixture(t *testing.T) orgFixture {
	t.Helper()

	root := t.TempDir()
	st := store.New(root)

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	ledger.StartRun()

	return orgFixture{
		collector: orgscope.New(collect.NewEnv(nil, st, ledger), "example-org"),
		store:     st,
		ledger:    ledger,
	}
}

// newOrgFixtureServer builds an org-scoped collector over a client pointed at a
// fake server serving mux, so the archive paths that fetch can be exercised end
// to end. The ledger reads its clock from now, fixing the fetch time every
// recorded outcome carries.
func newOrgFixtureServer(t *testing.T, mux *http.ServeMux, now time.Time) orgFixture {
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

	ledger, err := manifest.Load(st.Root(), manifest.WithClock(func() time.Time { return now }))
	require.NoError(t, err)

	ledger.StartRun()

	// A zero confirm delay keeps the 404-confirming re-probe from sleeping in
	// tests.
	env := collect.NewEnv(client, st, ledger, collect.WithAbsentConfirm(0))

	return orgFixture{
		collector: orgscope.New(env, "example-org"),
		store:     st,
		ledger:    ledger,
	}
}

// primaryDoc is a decoded jsonapi single-primary document, enough to assert an
// archived sub-object kept its own type and attributes rather than collapsing to
// a bare id ref.
type primaryDoc struct {
	Data struct {
		Type       string         `json:"type"`
		ID         string         `json:"id"`
		Attributes map[string]any `json:"attributes"`
	} `json:"data"`
}

// assertPrimary asserts relPath is recorded done and holds a jsonapi
// single-primary document of the given type and id, returning its attributes for
// further assertions. A populated attributes map is the proof the sub-object was
// archived in full rather than flattened to a bare {type, id} reference.
func (f orgFixture) assertPrimary(t *testing.T, relPath, wantType, wantID string) map[string]any {
	t.Helper()

	entry, ok := f.ledger.Entry(relPath)
	require.True(t, ok, "%s should have a ledger entry", relPath)
	assert.Equal(t, manifest.StatusDone, entry.Status, "%s should be recorded done", relPath)

	got, err := os.ReadFile(f.store.AbsPath(relPath))
	require.NoError(t, err, "%s should be written", relPath)

	var doc primaryDoc

	require.NoError(t, json.Unmarshal(got, &doc), "%s should be a jsonapi document", relPath)
	assert.Equal(t, wantType, doc.Data.Type, "%s jsonapi type", relPath)
	assert.Equal(t, wantID, doc.Data.ID, "%s jsonapi id", relPath)

	return doc.Data.Attributes
}

// assertNoEntry asserts the archive recorded nothing for relPath: neither a
// ledger entry nor a file.
func (f orgFixture) assertNoEntry(t *testing.T, relPath string) {
	t.Helper()

	_, ok := f.ledger.Entry(relPath)
	assert.False(t, ok, "%s should have no ledger entry", relPath)

	exists, err := f.store.Exists(relPath)
	require.NoError(t, err)
	assert.False(t, exists, "%s should not be written", relPath)
}
