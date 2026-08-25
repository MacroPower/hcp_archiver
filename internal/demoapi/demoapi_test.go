package demoapi_test

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/internal/demoapi"
	"go.jacobcolvin.com/hcp_archiver/pkg/archiver"
	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
)

// The organization every archiving test collects: small enough to walk in a
// few seconds, deep enough that both walks page and both physical forms appear.
const (
	testRuns   = 4
	testStates = 5
)

// The object counts a clean collection of that organization settles.
//
// They are the test's guard against a silent truncation: a listing served
// without its pagination object stops at page one and still records itself
// complete, so only a count catches it.
const (
	// The wantDone count is every object the run archived.
	wantDone = 415
	// The wantAbsent count is the organization's deliberate gaps: one expired
	// plan log in each of the seven workspaces, plus the one workspace with no
	// readme.
	wantAbsent = 8
)

// start binds a listener, serves the demo organization on it, and returns the
// base address the archiver points at. The server stops when the test ends.
func start(t *testing.T, opts ...demoapi.Option) (*demoapi.Server, string) {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := demoapi.New(opts...)
	ctx, cancel := context.WithCancel(t.Context())

	var wg sync.WaitGroup

	wg.Go(func() {
		assert.NoError(t, srv.Serve(ctx, ln))
	})

	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	addr := "http://" + ln.Addr().String()
	waitReady(t, addr)

	return srv, addr
}

// waitReady blocks until the server answers its ping, so a test never races the
// listener into service.
func waitReady(t *testing.T, addr string) {
	t.Helper()

	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, addr+"/api/v2/ping", http.NoBody)
		if err != nil {
			return false
		}

		req.Header.Set("Authorization", "Bearer demo")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}

		defer resp.Body.Close() //nolint:errcheck // The body is empty and the status is all this reads.

		return resp.StatusCode < http.StatusInternalServerError
	}, 10*time.Second, 20*time.Millisecond)
}

// collect runs one archive pass against addr into dir, returning the settled
// ledger's per-status counts and the run's own outcome.
func collect(t *testing.T, addr, dir string) (manifest.Tally, error) {
	t.Helper()

	cfg, err := config.New(
		config.WithToken("demo"),
		config.WithAddress(addr),
		config.WithArchiveDir(dir),
		config.WithOrganizations([]string{orgName}),
		config.WithProgressMode(config.ProgressModeQuiet),
		config.WithRateLimit(200),
	)
	require.NoError(t, err)

	logs := &strings.Builder{}
	a := archiver.New(cfg,
		archiver.WithWriter(io.Discard),
		archiver.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))),
	)

	runErr := a.Run(t.Context())

	if logs.Len() > 0 {
		t.Logf("archive warnings:\n%s", logs.String())
	}

	return tallyOf(t, dir), runErr
}

// orgName is the organization the server serves.
const orgName = "jacobcolvin-com"

// tallyOf reads the per-status counts the archive's ledger settled. The
// per-run counters (retries, bytes) are not among them: they live only in the
// run that recorded them.
func tallyOf(t *testing.T, dir string) manifest.Tally {
	t.Helper()

	ledger, err := manifest.Load(filepath.Join(dir, orgName))
	require.NoError(t, err)

	tally := ledger.Tally()

	// The ledger holds a cross-process lock over the archive root, so it is
	// released before the caller's next pass reopens it.
	require.NoError(t, ledger.Close())

	return tally
}

// historySidecars returns the history sidecars written beneath dir, one per
// object whose content a re-read superseded.
func historySidecars(t *testing.T, dir string) []string {
	t.Helper()

	var found []string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() && strings.HasSuffix(d.Name(), ".history.ndjson") {
			found = append(found, path)
		}

		return nil
	})
	require.NoError(t, err)

	return found
}

func TestArchiveIsClean(t *testing.T) {
	t.Parallel()

	_, addr := start(t, demoapi.WithRuns(testRuns), demoapi.WithStates(testStates))

	tally, err := collect(t, addr, t.TempDir())
	require.NoError(t, err, "a clean collection exits zero")

	assert.Zero(t, tally.SurfacesDropped, "every listing the collectors walk completed")
	assert.Zero(t, tally.Errored, "no object failed")
	assert.Zero(t, tally.Forbidden, "no object was denied")
	assert.Equal(t, wantDone, tally.Done, "the run archived every object it settles")
	assert.Equal(t, wantAbsent, tally.Absent, "only the deliberate gaps are absent")
}

func TestSecondRunRewritesNothing(t *testing.T) {
	t.Parallel()

	_, addr := start(t, demoapi.WithRuns(testRuns), demoapi.WithStates(testStates))
	dir := t.TempDir()

	_, err := collect(t, addr, dir)
	require.NoError(t, err)

	// The second pass is not a no-op by construction: the newest run is still
	// planning and the newest state version still pending, so both are re-read
	// every time. Nothing they carry changed, so nothing supersedes.
	tally, err := collect(t, addr, dir)
	require.NoError(t, err)

	assert.Equal(t, wantDone, tally.Done, "a resumed run settles the same objects")
	assert.Equal(t, wantAbsent, tally.Absent)
	assert.Zero(t, tally.Errored)
	assert.Empty(t, historySidecars(t, dir), "no object's content was superseded")
}

func TestChaosIsRecovered(t *testing.T) {
	t.Parallel()

	srv, addr := start(t,
		demoapi.WithRuns(testRuns),
		demoapi.WithStates(testStates),
		demoapi.WithChaos(true),
	)

	start := time.Now()

	tally, err := collect(t, addr, t.TempDir())
	require.NoError(t, err, "the injected failures are all recoverable")

	assert.Equal(t, 1, srv.RateLimited(),
		"exactly one rate-limited answer, so the governor halves its rate once and recovers")

	// The injected failures leave no trace in the settled ledger: the archiver
	// retried each of them in-run, so the counts match a collection that met
	// none of them.
	assert.Zero(t, tally.Errored, "every injected failure was retried into a success")
	assert.Zero(t, tally.SurfacesDropped)
	assert.Equal(t, wantDone, tally.Done)
	assert.Equal(t, wantAbsent, tally.Absent)

	// A governor that kept halving would take minutes over these few hundred
	// requests rather than the seconds the runs-list bucket already floors them
	// at, so the elapsed time is what proves the rate recovered.
	assert.Less(t, time.Since(start), time.Minute, "the admitted rate recovered after the single 429")
}

func TestUnauthorizedWithoutAToken(t *testing.T) {
	t.Parallel()

	_, addr := start(t, demoapi.WithRuns(1), demoapi.WithStates(1))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, addr+"/api/v2/organizations", http.NoBody)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close() //nolint:errcheck // The status is all this reads.

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestDecide(t *testing.T) {
	t.Parallel()

	const key = "/api/v2/state-versions/sv-example"

	tests := map[string]struct {
		profile  demoapi.Profile
		attempt  int
		status   int
		reset    string
		truncate bool
		delayed  bool
	}{
		"an untouched path is never delayed": {
			profile: demoapi.ProfileNone,
		},
		"an api path is only delayed": {
			profile: demoapi.ProfileAPI,
			delayed: true,
		},
		"a blob path is only delayed": {
			profile: demoapi.ProfileBlob,
			delayed: true,
		},
		"the first rate-limited attempt is refused": {
			profile: demoapi.ProfileRateLimit,
			status:  http.StatusTooManyRequests,
			reset:   "1.0",
			delayed: true,
		},
		"the retry of a rate-limited path succeeds": {
			profile: demoapi.ProfileRateLimit,
			attempt: 1,
			delayed: true,
		},
		"the first attempt at a truncated path is cut short": {
			profile:  demoapi.ProfileTruncate,
			truncate: true,
			delayed:  true,
		},
		"the retry of a truncated path succeeds": {
			profile: demoapi.ProfileTruncate,
			attempt: 1,
			delayed: true,
		},
		"the first attempt at a vanished path is missing": {
			profile: demoapi.ProfileVanish,
			status:  http.StatusNotFound,
			delayed: true,
		},
		"the retry of a vanished path succeeds": {
			profile: demoapi.ProfileVanish,
			attempt: 1,
			delayed: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := demoapi.Decide(7, key, tt.attempt, tt.profile)

			assert.Equal(t, tt.status, got.Status)
			assert.Equal(t, tt.reset, got.Reset)
			assert.Equal(t, tt.truncate, got.Truncate)

			if !tt.delayed {
				assert.Zero(t, got.Latency)

				return
			}

			bounds := demoapi.LatencyBounds[tt.profile]
			assert.GreaterOrEqual(t, got.Latency, bounds[0])
			assert.Less(t, got.Latency, bounds[1])

			assert.Equal(t, got, demoapi.Decide(7, key, tt.attempt, tt.profile),
				"the verdict is a pure function of its inputs")
		})
	}
}

func TestWindowFor(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		total  int
		number int
		size   int
		want   demoapi.Window
		page   []int
	}{
		"an empty listing still reports one page": {
			total: 0, number: 1, size: 20,
			want: demoapi.Window{CurrentPage: 1, TotalPages: 1},
			page: []int{},
		},
		"one short page advertises no next": {
			total: 3, number: 1, size: 20,
			want: demoapi.Window{CurrentPage: 1, TotalCount: 3, TotalPages: 1},
			page: []int{0, 1, 2},
		},
		"a full first page advertises the next": {
			total: 5, number: 1, size: 2,
			want: demoapi.Window{CurrentPage: 1, NextPage: 2, TotalCount: 5, TotalPages: 3},
			page: []int{0, 1},
		},
		"a middle page advertises both neighbors": {
			total: 5, number: 2, size: 2,
			want: demoapi.Window{CurrentPage: 2, PreviousPage: 1, NextPage: 3, TotalCount: 5, TotalPages: 3},
			page: []int{2, 3},
		},
		"the last page advertises no next": {
			total: 5, number: 3, size: 2,
			want: demoapi.Window{CurrentPage: 3, PreviousPage: 2, TotalCount: 5, TotalPages: 3},
			page: []int{4},
		},
		"a one-item probe reports the whole count": {
			total: 40, number: 1, size: 1,
			want: demoapi.Window{CurrentPage: 1, NextPage: 2, TotalCount: 40, TotalPages: 40},
			page: []int{0},
		},
		"a page past the end clamps to the last": {
			total: 5, number: 9, size: 2,
			want: demoapi.Window{CurrentPage: 3, PreviousPage: 2, TotalCount: 5, TotalPages: 3},
			page: []int{4},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			win, page := demoapi.WindowFor(tt.total, tt.number, tt.size)

			assert.Equal(t, tt.want, win)
			assert.Equal(t, tt.page, page)
		})
	}
}
