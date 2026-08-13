package workspace_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/collect/workspace"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

func TestRunTerminal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status tfe.RunStatus
		want   bool
	}{
		"applied is terminal":               {status: tfe.RunApplied, want: true},
		"planned and finished is terminal":  {status: tfe.RunPlannedAndFinished, want: true},
		"discarded is terminal":             {status: tfe.RunDiscarded, want: true},
		"errored is terminal":               {status: tfe.RunErrored, want: true},
		"canceled is terminal":              {status: tfe.RunCanceled, want: true},
		"policy soft failed is terminal":    {status: tfe.RunPolicySoftFailed, want: true},
		"policy override is not terminal":   {status: tfe.RunPolicyOverride, want: false},
		"force canceled is terminal":        {status: tfe.RunStatus("force_canceled"), want: true},
		"pending is not terminal":           {status: tfe.RunPending, want: false},
		"planning is not terminal":          {status: tfe.RunPlanning, want: false},
		"planned is not terminal":           {status: tfe.RunPlanned, want: false},
		"applying is not terminal":          {status: tfe.RunApplying, want: false},
		"planned and saved is not terminal": {status: tfe.RunPlannedAndSaved, want: false},
		"confirmed is not terminal":         {status: tfe.RunConfirmed, want: false},
		"empty status is not terminal":      {status: tfe.RunStatus(""), want: false},
		"unknown status is not terminal":    {status: tfe.RunStatus("surprise"), want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, tfeclient.RunTerminal(tc.status))
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

	assert.True(t, f.ledger.Collection("projects/proj/workspaces/ws/runs").Complete(),
		"a limit-stopped walk records completion so the seal phase can bundle the slice")
	assert.False(t, f.ledger.Collection("projects/proj/workspaces/ws/runs").Settled(),
		"but withholds settlement so a wider limit still pages down into the tail")
}

// TestCollectRunsBypassesTheGeneralGate guards the one call site the runs-list
// bucket split rests on. The runs list endpoint is paced sixty times slower than
// the general one, and a gate slot is held across that wait, so the walk must
// take its slot from the client's runs-list gate. Routing it through the general
// gate leaves the pacing correct, so nothing observable changes except a slot
// parked where the rest of the run needs it; this test asserts the slot.
func TestCollectRunsBypassesTheGeneralGate(t *testing.T) {
	t.Parallel()

	listed := make(chan struct{}, 1)

	mux := http.NewServeMux()
	// A non-blocking signal: a second request (a grown payload, a retry) must
	// report and move on rather than wedge the handler on a full channel, which
	// would hang the server's shutdown instead of failing readably.
	mux.HandleFunc("/api/v2/workspaces/ws-1/runs", func(w http.ResponseWriter, _ *http.Request) {
		select {
		case listed <- struct{}{}:
		default:
		}

		writeJSONAPI(t, w, runListPayload)
	})

	// A gate with no slots never grants one, standing in for a general gate the
	// rest of the run has fully occupied.
	f := newWSFixtureClient(t, mux, []tfeclient.Option{tfeclient.WithGate(tfeclient.NewSemaphore(0))}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ws := &tfe.Workspace{ID: "ws-1", Name: "ws"}

	var wg sync.WaitGroup

	// The listed runs are all in flight, so archiving them writes summaries
	// locally and issues no further request. Only the list request is under
	// test; the cancel below unwinds a walk that has not returned yet.
	wg.Go(func() {
		//nolint:errcheck // The walk returns nil, or a cancellation if the cancel below wins.
		_ = f.collector.CollectRuns(ctx, "proj", ws, nil)
	})

	select {
	case <-listed:
	case <-time.After(5 * time.Second):
		t.Fatal("the runs list request queued behind the general gate")
	}

	cancel()
	wg.Wait()
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
