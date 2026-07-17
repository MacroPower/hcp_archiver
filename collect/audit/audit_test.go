package audit_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
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
	return f.ledger.Collection(f.store.AuditTrailDir()).HighWaterMark()
}

// pageIDs decodes the archived page file written for the since cursor and page
// number and returns the ids of the events it holds.
func (f auditFixture) pageIDs(t *testing.T, since time.Time, page int) []string {
	t.Helper()

	return pageEventIDs(t, f.store, since, page)
}

// pageEventIDs decodes the archived page file at the since cursor and page number
// and returns the ids of the events it holds, the shared body of the fixtures'
// page assertions.
func pageEventIDs(t *testing.T, st *store.Store, since time.Time, page int) []string {
	t.Helper()

	relPath := st.AuditTrailFile(audit.PageName(since, page))

	raw, err := os.ReadFile(st.AbsPath(relPath))
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

	// A list error is a dropped surface, not a per-page outcome: the page slot is
	// a synthetic cursor position, not a resource, so it must be left unsettled
	// rather than recorded through Object.
	relPath := f.store.AuditTrailFile(audit.PageName(time.Time{}, 1))
	_, ok := f.ledger.Entry(relPath)
	assert.False(t, ok, "a list error must not settle the page slot")
	assert.True(t, f.ledger.ShouldFetch(relPath),
		"the slot stays fetchable so the next run re-lists and writes it")

	assert.Equal(t, 1, f.ledger.Tally().SurfacesDropped,
		"the unreached tail of the trail is a dropped surface")
}

func TestCollectTrailsDoesNotSettlePageOnTerminalListError(t *testing.T) {
	t.Parallel()

	// A 404 classifies terminal. Were the list error routed through Object it
	// would settle the page slot absent and short-circuit it on every later run,
	// so the real events a later list returns for the slot would never be written
	// and the walk would wedge on the never-created file. The slot must be left
	// unsettled instead.
	f := newAuditFixture(t, map[int]trailPage{
		1: {status: http.StatusNotFound},
	})

	require.NoError(t, f.collector.CollectTrails(t.Context()),
		"a list failure is best-effort, not a collector error")

	assert.True(t, f.watermark().IsZero(),
		"the cursor must hold so the next run retries the unread pages")

	relPath := f.store.AuditTrailFile(audit.PageName(time.Time{}, 1))
	_, ok := f.ledger.Entry(relPath)
	assert.False(t, ok, "a terminal list error must not settle the page slot absent")
	assert.True(t, f.ledger.ShouldFetch(relPath),
		"the slot stays fetchable so the next run re-lists and writes it")

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

	f.ledger.Collection(f.store.AuditTrailDir()).AdvanceHighWaterMark(mark)

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

func TestCollectTrailsEmptyPageDoesNotEndTheWalk(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	// An empty middle page must not end the walk. The trail's page order is
	// unspecified, so a later page can still carry an older event that would be
	// dropped if the empty page short-circuited the walk.
	f := newAuditFixture(t, map[int]trailPage{
		1: {events: []*tfe.AuditTrail{event("ev-new", base.Add(5*time.Second))}, nextPage: 2},
		2: {events: nil, nextPage: 3},
		3: {events: []*tfe.AuditTrail{event("ev-old", base.Add(1*time.Second))}, nextPage: 0},
	})

	require.NoError(t, f.collector.CollectTrails(t.Context()))

	assert.Equal(t, []string{"ev-new"}, f.pageIDs(t, time.Time{}, 1))
	assert.Equal(t, []string{"ev-old"}, f.pageIDs(t, time.Time{}, 3),
		"a page past an empty one is still walked and archived")
	assert.Equal(t, base.Add(5*time.Second), f.watermark(),
		"the watermark tracks the newest event across every walked page")
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

	f2.ledger.Collection(f2.store.AuditTrailDir()).AdvanceHighWaterMark(base)

	require.NoError(t, f2.collector.CollectTrails(t.Context()))

	assert.Equal(t, []string{"ev-2"}, f2.pageIDs(t, base, 1),
		"the resumed walk archives only events past the prior watermark")
	assert.Equal(t, base.Add(time.Minute), f2.watermark())
}

// resumeFixture serves audit-trail pages from a swappable table over a single
// store and ledger, so a multi-run resume that re-lists an already-settled page
// under an unchanged cursor can be exercised across runs.
type resumeFixture struct {
	collector *audit.Collector
	store     *store.Store
	ledger    *manifest.Ledger

	mu    sync.Mutex
	pages map[int]trailPage
}

// newResumeFixture builds a resume fixture over a fresh store and ledger, its
// page table starting empty and set per run with setPages.
func newResumeFixture(t *testing.T) *resumeFixture {
	t.Helper()

	f := &resumeFixture{pages: map[int]trailPage{}}

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

		p, ok := f.page(page)
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

	f.collector = audit.New(collect.NewEnv(client, st, ledger), "acme")
	f.store = st
	f.ledger = ledger

	return f
}

// setPages replaces the table the server serves for the next run.
func (f *resumeFixture) setPages(pages map[int]trailPage) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pages = pages
}

// page returns the served response for a page number under the lock.
func (f *resumeFixture) page(n int) (trailPage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	p, ok := f.pages[n]

	return p, ok
}

// run starts a fresh run and walks the trail, like a re-invocation would.
func (f *resumeFixture) run(t *testing.T) {
	t.Helper()

	f.ledger.StartRun()
	require.NoError(t, f.collector.CollectTrails(t.Context()))
}

// watermark reads the trail walk's Since cursor.
func (f *resumeFixture) watermark() time.Time {
	return f.ledger.Collection(f.store.AuditTrailDir()).HighWaterMark()
}

func TestCollectTrailsResumeDoesNotStepPastAnUnpersistedEvent(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	a := event("ev-a", base.Add(1*time.Second))
	b := event("ev-b", base.Add(2*time.Second))
	c := event("ev-c", base.Add(3*time.Second))

	f := newResumeFixture(t)

	// Run 1 archives page 1 = [A, B] then halts on a terminal list error at page
	// 2, so the watermark stays put and page 1 is left settled under the zero
	// cursor.
	f.setPages(map[int]trailPage{
		1: {events: []*tfe.AuditTrail{a, b}, nextPage: 2},
		2: {status: http.StatusBadRequest},
	})
	f.run(t)

	require.True(t, f.watermark().IsZero(), "the halted run does not advance the cursor")
	require.Equal(t, []string{"ev-a", "ev-b"}, pageEventIDs(t, f.store, time.Time{}, 1))

	// Between runs event C (> B) arrives, so run 2 re-lists page 1 = [A, B, C]
	// under the still-zero cursor and completes. Page 1 is already Done, so
	// Object skips the re-list and C is not persisted this run; the watermark
	// must advance only to the newest persisted event (B), never past C.
	f.setPages(map[int]trailPage{
		1: {events: []*tfe.AuditTrail{a, b, c}, nextPage: 0},
	})
	f.run(t)

	assert.Equal(t, b.Timestamp, f.watermark(),
		"the watermark advances to the newest persisted event, not the re-listed one")
	assert.Equal(t, []string{"ev-a", "ev-b"}, pageEventIDs(t, f.store, time.Time{}, 1),
		"the already-settled page keeps its stored events; C was not persisted")

	// Run 3 re-lists under the advanced cursor B, so page 1 is a fresh name and C
	// is now strictly newer than the cursor: it is finally persisted.
	f.run(t)

	assert.Equal(t, []string{"ev-c"}, pageEventIDs(t, f.store, b.Timestamp, 1),
		"C is captured on the run after the watermark advanced past B")
	assert.Equal(t, c.Timestamp, f.watermark())
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
