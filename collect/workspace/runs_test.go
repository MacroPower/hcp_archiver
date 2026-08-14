package workspace_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
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

// runShiftListing is a live runs listing served two at a time, which drops one
// run once the walk has fetched its first page: the upstream deletion that
// shifts every later run up a slot while the walk is busy archiving the page it
// already has. All the runs are in flight, so archiving one writes its summary
// and issues no further request.
type runShiftListing struct {
	drop    string
	ids     []string
	mu      sync.Mutex
	fetched bool
}

// serve answers one page of the listing, landing the pending deletion in the
// window after the first page has been fetched.
func (l *runShiftListing) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.fetched && l.drop != "" {
		l.ids = slices.DeleteFunc(l.ids, func(id string) bool { return id == l.drop })
		l.drop = ""
	}

	l.fetched = true

	const size = 2

	page, err := strconv.Atoi(r.URL.Query().Get("page[number]"))
	if err != nil || page < 1 {
		page = 1
	}

	start := min((page-1)*size, len(l.ids))
	end := min(start+size, len(l.ids))

	const run = `{"id":%q,"type":"runs","attributes":{"created-at":"2026-07-08T01:00:00Z","status":"planning"}}`

	items := make([]string, 0, size)
	for _, id := range l.ids[start:end] {
		items = append(items, fmt.Sprintf(run, id))
	}

	next := "null"
	if end < len(l.ids) {
		next = strconv.Itoa(page + 1)
	}

	writeJSONAPI(t, w, fmt.Sprintf(
		`{"data":[%s],"meta":{"pagination":{"current-page":%d,"next-page":%s,"total-count":%d}}}`,
		strings.Join(items, ","), page, next, len(l.ids)))
}

// TestCollectRunsClosesAListingShiftGap walks a workspace whose runs listing
// loses a run between the first page and the second. Under a plain page-number
// pager the deletion pulls the run that would have led page 2 into the range
// page 1 already covered, so it is never listed: no ledger entry, nothing for
// the unsettled-child scan to find, and a collection recorded complete and
// settled over the gap.
func TestCollectRunsClosesAListingShiftGap(t *testing.T) {
	t.Parallel()

	live := &runShiftListing{
		ids:  []string{"run-5", "run-4", "run-3", "run-2", "run-1"},
		drop: "run-5",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/workspaces/ws-1/runs", func(w http.ResponseWriter, r *http.Request) {
		live.serve(t, w, r)
	})

	// The walk fetches the listing several times over: the pages themselves plus
	// the re-list the shift forces. A loose ceiling on the runs-list governor
	// keeps the test off its production pacing of thirty requests a minute.
	f := newWSFixtureClient(t, mux, []tfeclient.Option{tfeclient.WithRunsListRateLimit(1000)}, nil)

	err := f.collector.CollectRuns(t.Context(), "proj", &tfe.Workspace{ID: "ws-1", Name: "ws"}, nil)
	require.NoError(t, err)

	// The deleted run-5 is archived from the page fetched before it went away,
	// and run-3 is the run the shift would otherwise have skipped.
	for _, id := range []string{"run-5", "run-4", "run-3", "run-2", "run-1"} {
		assert.Equal(t, manifest.StatusDone, f.status("projects/proj/workspaces/ws/runs/"+id+"/run.json"),
			"%s should be archived despite the listing shifting mid-walk", id)
	}
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
