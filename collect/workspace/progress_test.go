package workspace_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// svListPayload is a two-item state-version listing, newest first, whose
// finalized items carry no download URLs so archiving them needs no blob
// endpoints.
const svListPayload = `{
  "data": [
    {"id":"sv-2","type":"state-versions","attributes":{"created-at":"2026-07-08T13:00:00Z","status":"finalized"}},
    {"id":"sv-1","type":"state-versions","attributes":{"created-at":"2026-07-08T12:00:00Z","status":"finalized"}}
  ],
  "meta":{"pagination":{"current-page":1,"prev-page":null,"next-page":null,"total-pages":1,"total-count":2}}
}`

// newProgressServer serves the ping, a state-version listing with the given
// payload, an empty run listing, and 404 for everything else. A 404 on a
// detail endpoint records the object absent and moves on, so the sparse mux
// exercises the walks without modeling every endpoint.
func newProgressServer(t *testing.T, svPayload string) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v2/state-versions", func(w http.ResponseWriter, r *http.Request) {
		// Detail reads (/state-versions/{id}) fall through to the catch-all 404.
		if strings.HasPrefix(r.URL.Path, "/api/v2/state-versions/") {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		writeJSONAPI(t, w, svPayload)
	})
	mux.HandleFunc("/api/v2/workspaces/ws-1/runs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, listPayload(0))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

func TestCollectStateVersionsAdvancesProgressPerVersion(t *testing.T) {
	t.Parallel()

	srv := newProgressServer(t, svListPayload)
	collector := newCollector(t, srv)

	ws := &tfe.Workspace{ID: "ws-1", Name: "ws"}

	var calls []int

	err := collector.CollectStateVersions(t.Context(), "proj", ws, func(n int) {
		calls = append(calls, n)
	})
	require.NoError(t, err)

	assert.Equal(t, []int{1, 1}, calls, "each state version advances one unit as it lands")
}

func TestCollectWorkspaceAdvancesSettingsUnit(t *testing.T) {
	t.Parallel()

	// Both walks list empty, so the only unit that can advance is the settings
	// unit committed after the settings pass, the one that shows life before the
	// walks begin. The sparse mux 404s every settings detail endpoint; those
	// record absent without failing the pass, so the unit still lands.
	srv := newProgressServer(t, listPayload(0))
	collector := newCollector(t, srv)

	// Global remote state skips the consumers listing, the one settings surface
	// that would need another list endpoint.
	ws := &tfe.Workspace{ID: "ws-1", Name: "ws", GlobalRemoteState: true}

	var calls []int

	err := collector.CollectWorkspace(t.Context(), "proj", ws, func(n int) {
		calls = append(calls, n)
	})
	require.NoError(t, err)

	assert.Equal(t, []int{1}, calls, "completing the settings pass advances exactly one unit")
}
