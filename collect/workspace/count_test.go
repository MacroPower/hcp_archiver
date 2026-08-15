package workspace_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/hashicorp/go-tfe/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/collect/workspace"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// listPayload renders an empty JSON:API list carrying a pagination meta with
// the given total, the shape a PageSize-1 probe reads its count from.
func listPayload(total int) string {
	return fmt.Sprintf(
		`{"data":[],"meta":{"pagination":{"current-page":1,"prev-page":null,"next-page":null,"total-pages":%d,"total-count":%d}}}`,
		total,
		total,
	)
}

// testOrg is the organization every fake-server collector archives.
const testOrg = "example-org"

// newCollector builds a workspace collector over a client pointed at srv, with
// a real store and ledger rooted in the test's temp dir, applying any
// collector options.
func newCollector(t *testing.T, srv *httptest.Server, opts ...workspace.Option) *workspace.Collector {
	t.Helper()

	client, err := tfeclient.New(tfeclient.WithToken("test-token"), tfeclient.WithAddress(srv.URL))
	require.NoError(t, err)

	st := store.New(t.TempDir())

	ledger, err := manifest.Load(st.Root())
	require.NoError(t, err)

	ledger.StartRun()

	return workspace.New(collect.NewEnv(client, st, ledger), testOrg, opts...)
}

// writeJSONAPI writes body with the JSON:API content type.
func writeJSONAPI(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	w.Header().Set("Content-Type", "application/vnd.api+json")

	_, err := w.Write([]byte(body))
	require.NoError(t, err)
}

func TestCounts(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		svStatus   int
		svTotal    int
		advertised int
		wantRuns   int
		wantSVs    int
	}{
		"the advertised run count and the probed state-version total": {
			svStatus: http.StatusOK, svTotal: 55,
			advertised: 31,
			wantRuns:   31, wantSVs: 55,
		},
		"state version probe failure falls back to zero": {
			svStatus:   http.StatusNotFound,
			advertised: 31,
			wantRuns:   31, wantSVs: 0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			mux.HandleFunc("/api/v2/ping", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			// The runs list endpoint has its own 30-requests-per-minute rate
			// bucket, so counting must not spend it; the run total comes from
			// the workspace's advertised RunsCount instead.
			mux.HandleFunc("/api/v2/workspaces/ws-1/runs", func(http.ResponseWriter, *http.Request) {
				t.Error("counting must not probe the rate-scarce runs list endpoint")
			})
			mux.HandleFunc("/api/v2/state-versions", func(w http.ResponseWriter, _ *http.Request) {
				if tc.svStatus != http.StatusOK {
					w.WriteHeader(tc.svStatus)

					return
				}

				writeJSONAPI(t, w, listPayload(tc.svTotal))
			})

			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			collector := newCollector(t, srv)

			ws := &tfe.Workspace{ID: "ws-1", Name: "ws", RunsCount: tc.advertised}

			runs, svs := collector.Counts(t.Context(), ws)

			assert.Equal(t, tc.wantRuns, runs)
			assert.Equal(t, tc.wantSVs, svs)
		})
	}
}

func TestCountsClampsToRunHistoryCount(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		oldest     time.Time
		count      int
		advertised int
		wantRuns   int
	}{
		"a count-only bound clamps the advertised total": {
			count: 10, advertised: 74,
			wantRuns: 10,
		},
		"an advertised total under the bound stands": {
			count: 100, advertised: 74,
			wantRuns: 74,
		},
		"an age bound keeps the advertised total": {
			count: 10, oldest: time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC), advertised: 74,
			wantRuns: 74,
		},
		"no bound keeps the advertised total": {
			advertised: 74,
			wantRuns:   74,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			mux.HandleFunc("/api/v2/ping", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			mux.HandleFunc("/api/v2/state-versions", func(w http.ResponseWriter, _ *http.Request) {
				writeJSONAPI(t, w, listPayload(0))
			})

			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			collector := newCollector(t, srv, workspace.WithRunHistoryLimit(tc.count, tc.oldest))

			ws := &tfe.Workspace{ID: "ws-1", Name: "ws", RunsCount: tc.advertised}

			runs, _ := collector.Counts(t.Context(), ws)
			assert.Equal(t, tc.wantRuns, runs)
		})
	}
}

func TestCountsProbeQueries(t *testing.T) {
	t.Parallel()

	// Go-tfe validates the state-version list options, so a probe missing the
	// organization and workspace filters errors on every call and its fallback
	// silently reports zero; this pins the wire-level query so a mis-built probe
	// cannot hide behind that fallback.
	var svQuery url.Values

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v2/workspaces/ws-1/runs", func(http.ResponseWriter, *http.Request) {
		t.Error("counting must not probe the rate-scarce runs list endpoint")
	})
	mux.HandleFunc("/api/v2/state-versions", func(w http.ResponseWriter, r *http.Request) {
		svQuery = r.URL.Query()

		writeJSONAPI(t, w, listPayload(1))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	collector := newCollector(t, srv)

	ws := &tfe.Workspace{ID: "ws-1", Name: "ws", RunsCount: 3}

	runs, svs := collector.Counts(t.Context(), ws)
	assert.Equal(t, 3, runs, "the run total is the advertised count, unprobed")
	assert.Equal(t, 1, svs)

	require.NotNil(t, svQuery, "the state-version probe reaches the listing")
	assert.Equal(t, "1", svQuery.Get("page[size]"), "the state-version probe requests a single item")
	assert.Equal(t, testOrg, svQuery.Get("filter[organization][name]"))
	assert.Equal(t, "ws", svQuery.Get("filter[workspace][name]"))
}
