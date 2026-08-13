package workspace_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/hashicorp/jsonapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/collect/workspace"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// wsFixture builds a workspace collector over a client pointed at a fake server,
// with a real store and ledger, so the run-child archive paths can be exercised
// end to end.
type wsFixture struct {
	collector *workspace.Collector
	store     *store.Store
	ledger    *manifest.Ledger
}

// newWSFixture serves mux (with a go-tfe construction ping added) and builds a
// collector over a client pointed at it, applying any collector options.
func newWSFixture(t *testing.T, mux *http.ServeMux, opts ...workspace.Option) wsFixture {
	t.Helper()

	return newWSFixtureLedger(t, mux, nil, opts...)
}

// newWSFixtureLedger is [newWSFixture] with explicit ledger options, for tests
// that model a retry-absent run or another non-default ledger mode.
func newWSFixtureLedger(
	t *testing.T,
	mux *http.ServeMux,
	ledgerOpts []manifest.Option,
	opts ...workspace.Option,
) wsFixture {
	t.Helper()

	return newWSFixtureClient(t, mux, nil, ledgerOpts, opts...)
}

// newWSFixtureClient is [newWSFixtureLedger] with explicit client options, for
// tests that need to shape the client's gates or rate governors.
func newWSFixtureClient(
	t *testing.T,
	mux *http.ServeMux,
	clientOpts []tfeclient.Option,
	ledgerOpts []manifest.Option,
	opts ...workspace.Option,
) wsFixture {
	t.Helper()

	mux.HandleFunc("/api/v2/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := tfeclient.New(append([]tfeclient.Option{
		tfeclient.WithToken("test-token"),
		tfeclient.WithAddress(srv.URL),
	}, clientOpts...)...)
	require.NoError(t, err)

	st := store.New(t.TempDir())

	ledger, err := manifest.Load(st.Root(), ledgerOpts...)
	require.NoError(t, err)

	ledger.StartRun()

	// A zero confirm delay keeps the 404-confirming re-probe from sleeping in
	// tests.
	env := collect.NewEnv(client, st, ledger, collect.WithAbsentConfirm(0))

	return wsFixture{
		collector: workspace.New(env, testOrg, opts...),
		store:     st,
		ledger:    ledger,
	}
}

// status returns the recorded ledger status for relPath, or the empty string
// when no entry exists.
func (f wsFixture) status(relPath string) manifest.Status {
	entry, ok := f.ledger.Entry(relPath)
	if !ok {
		return ""
	}

	return entry.Status
}

// attrs reads relPath, asserts it is recorded done, and returns the attributes of
// its jsonapi single-primary document. A populated map is the proof the
// sub-object was archived in full rather than flattened to a bare id ref.
func (f wsFixture) attrs(t *testing.T, relPath, wantType, wantID string) map[string]any {
	t.Helper()

	assert.Equal(t, manifest.StatusDone, f.status(relPath), "%s should be recorded done", relPath)

	got, err := os.ReadFile(f.store.AbsPath(relPath))
	require.NoError(t, err, "%s should be written", relPath)

	var doc struct {
		Data struct {
			Type       string         `json:"type"`
			ID         string         `json:"id"`
			Attributes map[string]any `json:"attributes"`
		} `json:"data"`
	}

	require.NoError(t, json.Unmarshal(got, &doc), "%s should be a jsonapi document", relPath)
	assert.Equal(t, wantType, doc.Data.Type, "%s jsonapi type", relPath)
	assert.Equal(t, wantID, doc.Data.ID, "%s jsonapi id", relPath)

	return doc.Data.Attributes
}

// preSettle records relPath done so a fetch for it is skipped, isolating a test
// on the object under exercise (e.g. settling a run's tarball to skip download).
func (f wsFixture) preSettle(relPath string) {
	f.ledger.RecordDone(relPath, manifest.Signature{Size: 1})
}

// dataIDs reads relPath, asserts it is recorded done, and returns the ids of its
// jsonapi list document in order.
func (f wsFixture) dataIDs(t *testing.T, relPath string) []string {
	t.Helper()

	assert.Equal(t, manifest.StatusDone, f.status(relPath), "%s should be recorded done", relPath)

	got, err := os.ReadFile(f.store.AbsPath(relPath))
	require.NoError(t, err, "%s should be written", relPath)

	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	require.NoError(t, json.Unmarshal(got, &doc), "%s should be a jsonapi list", relPath)

	ids := make([]string, len(doc.Data))
	for i, d := range doc.Data {
		ids[i] = d.ID
	}

	return ids
}

// marshalJSONAPI renders model as a JSON:API document, sideloading its hydrated
// relations into the included array the go-tfe client links back on read.
func marshalJSONAPI(t *testing.T, model any) string {
	t.Helper()

	var buf bytes.Buffer

	require.NoError(t, jsonapi.MarshalPayload(&buf, model))

	return buf.String()
}
