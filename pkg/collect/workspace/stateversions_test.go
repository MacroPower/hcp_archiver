package workspace_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
	"go.jacobcolvin.com/hcp_archiver/pkg/collect/workspace"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
	"go.jacobcolvin.com/hcp_archiver/pkg/tfeclient"
)

func TestStateVersionTerminal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status tfe.StateVersionStatus
		want   bool
	}{
		"pending is not terminal": {status: tfe.StateVersionPending, want: false},
		"finalized is terminal":   {status: tfe.StateVersionFinalized, want: true},
		"discarded is terminal":   {status: tfe.StateVersionDiscarded, want: true},
		// A pre-status server omits the attribute; its versions exist only once
		// uploaded, so an empty status is a finalized version.
		"empty status is terminal": {status: tfe.StateVersionStatus(""), want: true},
		// The allowlist polarity: a status added to the SDK after this list was
		// written must default to live, so a transiently-empty download URL is
		// re-walked rather than settled as a permanent absence.
		"unknown status is not terminal": {status: tfe.StateVersionStatus("surprise"), want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, tfeclient.StateVersionTerminal(tc.status))
		})
	}
}

func TestArchiveStateVersionPendingThenFinalized(t *testing.T) {
	t.Parallel()

	const rawBody = `{"version":4,"serial":1,"terraform_version":"1.9.0"}`

	client, rawURL, jsonURL := newStateDownloadClient(t, rawBody)

	st := store.New(t.TempDir())

	ledger, err := manifest.Load(st.Root())
	require.NoError(t, err)

	ledger.StartRun()

	env := collect.NewEnv(client, st, ledger)
	collector := workspace.New(env, "example-org")

	createdAt := time.Date(2026, time.July, 8, 12, 0, 0, 0, time.UTC)
	rawPath := st.StateVersionFile("proj", "ws", createdAt, "sv-1", "tfstate.json")
	metaPath := st.StateVersionFile("proj", "ws", createdAt, "sv-1", "meta.json")

	// Pre-settle the meta sidecar so the archive skips fetching it; the sidecar is
	// out of scope for this blob-focused fix, and settling it isolates the test on
	// the raw blob's pending-vs-finalized behavior.
	ledger.RecordDone(metaPath, manifest.Signature{Size: 1})

	// Pass 1: pending with empty download URLs. The blob's URL is only
	// transiently empty, so it must not be settled: no ledger entry and no file,
	// leaving ShouldFetch true for a later pass.
	pending := &tfe.StateVersion{ID: "sv-1", CreatedAt: createdAt, Status: tfe.StateVersionPending}
	require.NoError(t, collector.ArchiveStateVersion(t.Context(), "proj", "ws", pending))

	assert.False(t, tfeclient.StateVersionTerminal(pending.Status), "a pending state version is non-terminal")

	_, ok := ledger.Entry(rawPath)
	assert.False(t, ok, "a pending state version records no blob outcome")

	exists, err := st.Exists(rawPath)
	require.NoError(t, err)
	assert.False(t, exists, "no blob file is written while pending")

	// Pass 2: finalized with a populated URL. The blob is now fetched and
	// archived, recovering the state the pending pass deliberately deferred.
	finalized := &tfe.StateVersion{
		ID:              "sv-1",
		CreatedAt:       createdAt,
		Status:          tfe.StateVersionFinalized,
		DownloadURL:     rawURL,
		JSONDownloadURL: jsonURL,
	}
	require.NoError(t, collector.ArchiveStateVersion(t.Context(), "proj", "ws", finalized))

	entry, ok := ledger.Entry(rawPath)
	require.True(t, ok, "the finalized blob is recorded")
	assert.Equal(t, manifest.StatusDone, entry.Status)

	exists, err = st.Exists(rawPath)
	require.NoError(t, err)
	assert.True(t, exists, "the finalized blob is written")

	got, err := os.ReadFile(st.AbsPath(rawPath))
	require.NoError(t, err)
	assert.JSONEq(t, rawBody, string(got), "the archived blob is the downloaded state")
}

// newStateDownloadClient builds a client whose server answers the go-tfe
// construction ping and serves the raw and JSON state blobs, returning the
// client and the two full download URLs to place on a state version.
func newStateDownloadClient(t *testing.T, body string) (*tfeclient.Client, string, string) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, _ *http.Request) {
		_, werr := io.WriteString(w, body)
		if werr != nil {
			return
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := tfeclient.New(tfeclient.WithToken("test-token"), tfeclient.WithAddress(srv.URL))
	require.NoError(t, err)

	return client, srv.URL + "/dl/raw", srv.URL + "/dl/json"
}
