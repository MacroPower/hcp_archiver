package audit_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tfe "github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/collect/audit"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// trailPage is one canned response for a page number of the audit-trail
// endpoint: its events and the pagination it advertises.
type trailPage struct {
	events   []*tfe.AuditTrail
	nextPage int
	status   int
}

// auditFixture drives the trail walk against a fake audit-trail endpoint with a
// real store and ledger, so the watermark and halt decisions under test are the
// ones a run would make.
type auditFixture struct {
	collector *audit.Collector
	store     *store.Store
	ledger    *manifest.Ledger
}

// newAuditFixture serves pages keyed by page number (page 1 serves requests
// with no explicit page too) and builds the audit collector over a client
// pointed at the fake server.
func newAuditFixture(t *testing.T, pages map[int]trailPage) auditFixture {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v2/organization/audit-trail", func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if raw := r.URL.Query().Get("page[number]"); raw != "" {
			n, err := strconv.Atoi(raw)
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusBadRequest)

				return
			}

			page = n
		}

		p, ok := pages[page]
		if !assert.Truef(t, ok, "unexpected page %d requested", page) {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		if p.status != 0 && p.status != http.StatusOK {
			w.WriteHeader(p.status)

			return
		}

		body := map[string]any{
			"pagination": &tfe.AuditTrailPagination{CurrentPage: page, NextPage: p.nextPage},
			"data":       p.events,
		}

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(body))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := tfeclient.New(tfeclient.WithToken("test-token"), tfeclient.WithAddress(srv.URL))
	require.NoError(t, err)

	st := store.New(t.TempDir())

	ledger, err := manifest.Load(st.Root())
	require.NoError(t, err)

	ledger.StartRun()

	env := collect.NewEnv(client, st, ledger)

	return auditFixture{
		collector: audit.New(env, "acme"),
		store:     st,
		ledger:    ledger,
	}
}

// watermark reads the trail walk's Since cursor.
func (f auditFixture) watermark() time.Time {
	return f.ledger.HighWaterMark(f.store.AuditTrailDir())
}

// pageIDs decodes the archived page file written for the since cursor and page
// number and returns the ids of the events it holds.
func (f auditFixture) pageIDs(t *testing.T, since time.Time, page int) []string {
	t.Helper()

	relPath := f.store.AuditTrailFile(audit.PageName(since, page))

	raw, err := os.ReadFile(f.store.AbsPath(relPath))
	require.NoError(t, err, "%s should be written", relPath)

	var events []struct {
		ID string `json:"id"`
	}

	require.NoError(t, json.Unmarshal(raw, &events))

	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}

	return ids
}

// event builds an audit trail event with the id and timestamp under test.
func event(id string, ts time.Time) *tfe.AuditTrail {
	return &tfe.AuditTrail{ID: id, Type: "Resource", Timestamp: ts}
}

func TestCollectTrailsAdvancesWatermarkAfterCleanWalk(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	// Two pages; the newest event sits on the second page, so the watermark must
	// track the maximum timestamp, not the last page's or the first item's.
	f := newAuditFixture(t, map[int]trailPage{
		1: {events: []*tfe.AuditTrail{event("ev-1", base.Add(1*time.Second))}, nextPage: 2},
		2: {events: []*tfe.AuditTrail{event("ev-2", base.Add(5*time.Second))}, nextPage: 0},
	})

	require.NoError(t, f.collector.CollectTrails(t.Context()))

	assert.Equal(t, base.Add(5*time.Second), f.watermark(),
		"a clean full walk advances the cursor to the newest event seen")
	assert.Equal(t, []string{"ev-1"}, f.pageIDs(t, time.Time{}, 1))
	assert.Equal(t, []string{"ev-2"}, f.pageIDs(t, time.Time{}, 2))
	assert.Zero(t, f.ledger.Tally().SurfacesDropped)
}

func TestCollectTrailsHoldsWatermarkOnListError(t *testing.T) {
	t.Parallel()

	// A 400 fails the list terminally without engaging the client's
	// server-error retries, so the halt path under test runs immediately.
	f := newAuditFixture(t, map[int]trailPage{
		1: {status: http.StatusBadRequest},
	})

	require.NoError(t, f.collector.CollectTrails(t.Context()),
		"a list failure is best-effort, not a collector error")

	assert.True(t, f.watermark().IsZero(),
		"the cursor must hold so the next run retries the unread pages")

	entry, ok := f.ledger.Entry(f.store.AuditTrailFile(audit.PageName(time.Time{}, 1)))
	require.True(t, ok, "the failed page is recorded")
	assert.Equal(t, manifest.StatusErrored, entry.Status)

	assert.Equal(t, 1, f.ledger.Tally().SurfacesDropped,
		"the unreached tail of the trail is a dropped surface")
}

func TestCollectTrailsHaltsOnStalledPagination(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	// Page 1 keeps advertising itself as the next page: a cycle the walk cannot
	// pass. It must halt with the watermark unmoved rather than advance past the
	// pages the server still claims exist.
	f := newAuditFixture(t, map[int]trailPage{
		1: {events: []*tfe.AuditTrail{event("ev-1", base)}, nextPage: 1},
	})

	require.NoError(t, f.collector.CollectTrails(t.Context()))

	assert.True(t, f.watermark().IsZero(),
		"the cursor must hold: the claimed further pages were never reached")
	assert.Equal(t, []string{"ev-1"}, f.pageIDs(t, time.Time{}, 1),
		"the page that did list is still archived")
	assert.Equal(t, 1, f.ledger.Tally().SurfacesDropped)
}

func TestCollectTrailsDropsEventsAtOrBeforeSubSecondWatermark(t *testing.T) {
	t.Parallel()

	// The wire cursor is floored to whole seconds, so a resume from a sub-second
	// watermark re-lists the events already archived within the watermark's
	// second. Those must be dropped; only the strictly newer event is fresh.
	mark := time.Date(2026, 7, 10, 12, 0, 0, 500_000_000, time.UTC)

	f := newAuditFixture(t, map[int]trailPage{
		1: {events: []*tfe.AuditTrail{
			event("ev-new", mark.Add(300*time.Millisecond)),
			event("ev-at-mark", mark),
			event("ev-old", mark.Add(-200*time.Millisecond)),
		}, nextPage: 0},
	})

	f.ledger.AdvanceHighWaterMark(f.store.AuditTrailDir(), mark)

	require.NoError(t, f.collector.CollectTrails(t.Context()))

	assert.Equal(t, []string{"ev-new"}, f.pageIDs(t, mark, 1),
		"only the event strictly newer than the watermark is fresh")
	assert.Equal(t, mark.Add(300*time.Millisecond), f.watermark())
}

func TestCollectTrailsEmptyPageStopsWithoutWriting(t *testing.T) {
	t.Parallel()

	f := newAuditFixture(t, map[int]trailPage{
		1: {events: nil, nextPage: 0},
	})

	require.NoError(t, f.collector.CollectTrails(t.Context()))

	assert.True(t, f.watermark().IsZero(), "nothing seen, nothing advanced")

	relPath := f.store.AuditTrailFile(audit.PageName(time.Time{}, 1))
	_, ok := f.ledger.Entry(relPath)
	assert.False(t, ok,
		"an empty page must not settle its cursor-keyed name: events arriving "+
			"later under the same cursor would be skipped as already archived")
	assert.Zero(t, f.ledger.Tally().SurfacesDropped)
}

func TestCollectTrailsResumeAppendsOnlyNewerEvents(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	// First run: one event. Second run: the server re-lists it along with a
	// newer one. The runs' pages are keyed by their distinct cursors, so the
	// second run writes a fresh page holding only the new event.
	f := newAuditFixture(t, map[int]trailPage{
		1: {events: []*tfe.AuditTrail{event("ev-1", base)}, nextPage: 0},
	})

	require.NoError(t, f.collector.CollectTrails(t.Context()))
	require.Equal(t, base, f.watermark())

	f2 := newAuditFixture(t, map[int]trailPage{
		1: {events: []*tfe.AuditTrail{
			event("ev-2", base.Add(time.Minute)),
			event("ev-1", base),
		}, nextPage: 0},
	})

	f2.ledger.AdvanceHighWaterMark(f2.store.AuditTrailDir(), base)

	require.NoError(t, f2.collector.CollectTrails(t.Context()))

	assert.Equal(t, []string{"ev-2"}, f2.pageIDs(t, base, 1),
		"the resumed walk archives only events past the prior watermark")
	assert.Equal(t, base.Add(time.Minute), f2.watermark())
}

// TestPageNameDistinctAcrossSubSecondCursors pins the property the resume
// depends on: two walks whose cursors differ by less than a second still get
// distinct page names, so the later walk cannot settle its events under a name
// the earlier walk already recorded.
func TestPageNameDistinctAcrossSubSecondCursors(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 10, 12, 0, 0, 100_000_000, time.UTC)

	a := audit.PageName(base, 1)
	b := audit.PageName(base.Add(time.Nanosecond), 1)

	assert.NotEqualf(t, a, b, "cursors %s and %s must not collide", a, b)
}
