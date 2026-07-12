package workspace_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/collect/workspace"
	"go.jacobcolvin.com/hcp_archiver/manifest"
)

func TestRunTerminal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status tfe.RunStatus
		want   bool
	}{
		"applied is terminal":                {status: tfe.RunApplied, want: true},
		"planned and finished is terminal":   {status: tfe.RunPlannedAndFinished, want: true},
		"discarded is terminal":              {status: tfe.RunDiscarded, want: true},
		"errored is terminal":                {status: tfe.RunErrored, want: true},
		"canceled is terminal":               {status: tfe.RunCanceled, want: true},
		"policy soft failed is not terminal": {status: tfe.RunPolicySoftFailed, want: false},
		"force canceled is terminal":         {status: tfe.RunStatus("force_canceled"), want: true},
		"pending is not terminal":            {status: tfe.RunPending, want: false},
		"planning is not terminal":           {status: tfe.RunPlanning, want: false},
		"planned is not terminal":            {status: tfe.RunPlanned, want: false},
		"applying is not terminal":           {status: tfe.RunApplying, want: false},
		"planned and saved is not terminal":  {status: tfe.RunPlannedAndSaved, want: false},
		"confirmed is not terminal":          {status: tfe.RunConfirmed, want: false},
		"empty status is not terminal":       {status: tfe.RunStatus(""), want: false},
		"unknown status is not terminal":     {status: tfe.RunStatus("surprise"), want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, workspace.RunTerminal(tc.status))
		})
	}
}

// runListPayload is a three-run listing, newest first and all in flight, so
// archiving a run writes only its mutable summary and needs no child
// endpoints.
const runListPayload = `{
  "data": [
    {"id":"run-3","type":"runs","attributes":{"created-at":"2026-07-08T03:00:00Z","status":"planning"}},
    {"id":"run-2","type":"runs","attributes":{"created-at":"2026-07-08T02:00:00Z","status":"planning"}},
    {"id":"run-1","type":"runs","attributes":{"created-at":"2026-07-08T01:00:00Z","status":"planning"}}
  ],
  "meta":{"pagination":{"current-page":1,"prev-page":null,"next-page":null,"total-pages":1,"total-count":3}}
}`

func TestCollectRunsHonorsRunHistoryLimit(t *testing.T) {
	t.Parallel()

	// The count bound admits only the newest run while the age window reaches
	// back past the second, so the walk archives two runs and stops before the
	// oldest: the two bounds compose as whichever admits more history.
	oldest := time.Date(2026, time.July, 8, 2, 0, 0, 0, time.UTC)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/workspaces/ws-1/runs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, runListPayload)
	})

	f := newWSFixture(t, mux, workspace.WithRunHistoryLimit(1, oldest))

	ws := &tfe.Workspace{ID: "ws-1", Name: "ws"}

	err := f.collector.CollectRuns(t.Context(), "proj", ws, nil)
	require.NoError(t, err)

	assert.Equal(t, manifest.StatusDone, f.status("projects/proj/workspaces/ws/runs/run-3/run.json"))
	assert.Equal(t, manifest.StatusDone, f.status("projects/proj/workspaces/ws/runs/run-2/run.json"))

	_, ok := f.ledger.Entry("projects/proj/workspaces/ws/runs/run-1/run.json")
	assert.False(t, ok, "the run outside every bound is not archived")

	assert.True(t, f.ledger.IsCollectionComplete("projects/proj/workspaces/ws/runs"),
		"a limit-stopped walk records completion so the seal phase can bundle the slice")
	assert.False(t, f.ledger.IsCollectionSettled("projects/proj/workspaces/ws/runs"),
		"but withholds settlement so a wider limit still pages down into the tail")
}

func TestHasNextPage(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		page *tfe.Pagination
		want bool
	}{
		"nil pagination has no next":    {page: nil, want: false},
		"zero next page has no next":    {page: &tfe.Pagination{NextPage: 0}, want: false},
		"positive next page has a next": {page: &tfe.Pagination{NextPage: 2}, want: true},
		"current before total has next": {page: &tfe.Pagination{NextPage: 5}, want: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, workspace.HasNextPage(tc.page))
		})
	}
}
