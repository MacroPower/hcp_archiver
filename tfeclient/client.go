package tfeclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-tfe"
	"golang.org/x/sync/errgroup"
)

// DefaultRateLimit is the default ceiling of the general adaptive rate
// governor, in requests per second, shared across every worker using a
// [Client]. The governor starts at the ceiling and adapts downward from the
// server's rate-limit feedback, so this is the fastest the client will ever
// launch, not a promise of sustained throughput.
const DefaultRateLimit float64 = 30

// DefaultRunsListRateLimit is the default ceiling of the separate governor
// pacing the two runs list endpoints (/workspaces/:workspace_id/runs and
// /organizations/:name/runs), in requests per second. HCP Terraform meters
// those two endpoints in a bucket of their own at 30 requests per minute, sixty
// times below the general limit and documented only on the runs API page, so
// pacing them from the general governor farms 429s whose pauses stall every
// other endpoint. The default sits one request per minute under that budget: a
// steady rate of exactly 30 per minute still places up to 31 launches inside
// one sliding window (the fencepost request plus the bucket's one saved token),
// and a single tripped 429 costs up to a minute of cooldown on an
// already-scarce budget.
const DefaultRunsListRateLimit float64 = 29.0 / 60

// Pagination settings for [Paginate].
const (
	// MaxPageSize is the page size [Paginate] requests. The API serves at most
	// 100 items per page and defaults to 20, so asking for the maximum makes a
	// full enumeration cost a fifth of the round-trips.
	MaxPageSize = 100

	// DefaultPageConcurrency is the ceiling on how many pages one [Paginate]
	// call fetches at once. It sits below the client's in-flight gate, which
	// binds first, so it only keeps one listing from monopolizing the gate.
	DefaultPageConcurrency = 8
)

// DefaultResponseHeaderTimeout bounds the time [New]'s own HTTP client waits for
// a response's headers, so a stalled connection cannot wedge a worker (and with
// it the sequential org loop) forever. It bounds only the time to first byte,
// not the body, so a large streaming download is not capped.
const DefaultResponseHeaderTimeout = 60 * time.Second

// DefaultTLSHandshakeTimeout bounds a new connection's TLS handshake when [New]
// builds its own HTTP client. Log, state, and tarball downloads dial the
// artifact host over HTTP/1, so under a saturated link handshake packets queue
// behind the bulk transfers of concurrent workers; the bound sits above the
// transport's usual 10 seconds to tolerate that congestion while still failing
// a dead dial promptly.
const DefaultTLSHandshakeTimeout = 20 * time.Second

// Gate bounds how many requests may be in flight at once. Acquire blocks
// until a slot is free or ctx is done and Release returns the slot. The gate
// answers "how many at once" and nothing else; "how fast" belongs to the
// client's adaptive rate governor, so server feedback moves the rate, never
// the concurrency. The archiver satisfies it with a fixed-size counting
// semaphore.
type Gate interface {
	// Acquire takes a slot, blocking until one is free or ctx is done.
	Acquire(ctx context.Context) error
	// Release returns a slot taken by Acquire.
	Release()
}

// Client is the single, worker-safe point of contact with HCP Terraform.
//
// It holds exactly one underlying go-tfe client whose HTTP transport paces
// every outbound attempt through shared adaptive rate governors, so that N
// concurrent workers paginating and downloading share one aggregate throttle
// rather than each retrying in isolation. The governors live at the transport
// rather than the request level so nothing outruns them: the pages go-tfe's
// ListAll methods fetch internally, the fresh requests its chunked log readers
// make mid-stream, and every in-client retry all pay for themselves. There is
// one governor per server-side rate bucket: a general one for most endpoints,
// and a far slower one for the two runs list endpoints the server meters
// separately (see [DefaultRunsListRateLimit]). Each governor is its bucket's
// one rate authority: it starts at its ceiling, halves on the server's
// rate-limit pushback, pauses its bucket's launches until the server's
// advertised reset, and creeps back up while responses stay clean; a 429 in one
// bucket never slows the other. Go-tfe's own rate-limit machinery is kept
// dormant (its server-error retry disabled, a 429 never surfaced to it, and the
// rate-limit header its internal limiter reads stripped). Every request routed
// through [Client.Do] additionally takes a slot from the optional [Gate] first,
// so a caller can bound the whole run's parallelism in one place.
//
// Create instances with [New]. A Client is safe for concurrent use.
type Client struct {
	tfe      *tfe.Client
	gate     Gate
	governor *Governor
}

// config holds the resolved settings a [Client] is built from. The governors
// are derived from the rate limits by [newConfig] once the options are
// applied, so [resolveHTTPClient] can bind them into the transport.
type config struct {
	httpClient            *http.Client
	logger                *slog.Logger
	wireBytes             *atomic.Int64
	rateLimited           *atomic.Int64
	gate                  Gate
	governor              *Governor
	runsGovernor          *Governor
	address               string
	token                 string
	rateLimit             float64
	runsListRateLimit     float64
	responseHeaderTimeout time.Duration
	idleReadTimeout       time.Duration
	serverErrorRetryDelay time.Duration
	serverErrorRetries    int
}

// Option configures a [Client] during [New].
//
// Options of this type:
//   - [WithToken]
//   - [WithAddress]
//   - [WithRateLimit]
//   - [WithRunsListRateLimit]
//   - [WithResponseHeaderTimeout]
//   - [WithIdleReadTimeout]
//   - [WithServerErrorRetry]
//   - [WithWireBytes]
//   - [WithRateLimitCounter]
//   - [WithGate]
//   - [WithLogger]
//   - [WithHTTPClient]
type Option func(*config)

// WithToken sets the API token used to authenticate. It is required.
// Returns an [Option].
func WithToken(token string) Option {
	return func(c *config) {
		c.token = token
	}
}

// WithAddress sets the HCP Terraform / Terraform Enterprise base address.
// An empty value keeps the default of https://app.terraform.io.
// Returns an [Option].
func WithAddress(address string) Option {
	return func(c *config) {
		if address != "" {
			c.address = address
		}
	}
}

// WithRateLimit sets the general adaptive rate governor's ceiling, in
// requests per second. The governor starts at the ceiling and adapts downward
// from the server's rate-limit feedback, so this bounds the fastest the
// client will ever launch. The runs list endpoints are paced separately (see
// [WithRunsListRateLimit]). A non-positive value keeps [DefaultRateLimit].
// Returns an [Option].
func WithRateLimit(perSecond float64) Option {
	return func(c *config) {
		if perSecond > 0 {
			c.rateLimit = perSecond
		}
	}
}

// WithRunsListRateLimit sets the ceiling of the separate governor pacing the
// two runs list endpoints, in requests per second, for a server whose
// runs-list budget differs from HCP Terraform's documented 30 per minute. A
// non-positive value keeps [DefaultRunsListRateLimit]. Returns an [Option].
func WithRunsListRateLimit(perSecond float64) Option {
	return func(c *config) {
		if perSecond > 0 {
			c.runsListRateLimit = perSecond
		}
	}
}

// WithResponseHeaderTimeout bounds how long a request waits for the response
// headers (its time to first byte) when [New] builds its own HTTP client, so a
// stalled connection cannot hang a worker indefinitely. It does not bound the
// body transfer, so a large streaming state, log, or tarball download still
// runs uncapped; each body read's idle wait is bounded separately (see
// [WithIdleReadTimeout]). A non-positive value keeps
// [DefaultResponseHeaderTimeout], and a caller-supplied [WithHTTPClient] takes
// precedence over it. Returns an [Option].
func WithResponseHeaderTimeout(timeout time.Duration) Option {
	return func(c *config) {
		if timeout > 0 {
			c.responseHeaderTimeout = timeout
		}
	}
}

// WithIdleReadTimeout bounds how long a response body read may sit with no
// bytes arriving when [New] builds its own HTTP client, so a connection that
// stalls mid-body cannot hang a worker indefinitely. It is an idle bound, not
// a total one: a slow but live streaming download is never capped, only
// silence is. A stalled read fails with an error wrapping [ErrIdleReadTimeout]
// that classifies as [KindTransient], so a re-run retries the object. A
// non-positive value keeps [DefaultIdleReadTimeout], and a caller-supplied
// [WithHTTPClient] takes precedence over it. Returns an [Option].
func WithIdleReadTimeout(timeout time.Duration) Option {
	return func(c *config) {
		if timeout > 0 {
			c.idleReadTimeout = timeout
		}
	}
}

// WithServerErrorRetry sets how the client retries a request that failed at
// the transport or answered with a server error (5xx): retries is the number
// of additional attempts after the first, and delay is the wait before the
// first retry, doubling on each retry after that (capped internally, so the
// worst-case stall stays in seconds). Rate-limited (429) responses are not
// governed by this; they carry their own fixed budget
// ([DefaultRateLimitRetries]) and back off in the shared governor rather
// than locally. A non-positive retries disables server-error retrying,
// leaving the failure to the caller's recorded outcome and the next run; a
// non-positive delay retries immediately. Returns an [Option].
func WithServerErrorRetry(retries int, delay time.Duration) Option {
	return func(c *config) {
		c.serverErrorRetries = retries
		c.serverErrorRetryDelay = delay
	}
}

// WithWireBytes sets a shared counter that accumulates response-body bytes as
// they are delivered to the reader when [New] builds its own HTTP client, so a
// progress view can derive live throughput while a large transfer is still in
// flight. The tally is of delivered bytes after any transport-level
// decompression, not raw compressed bytes on the wire. A nil counter disables
// counting, and a caller-supplied [WithHTTPClient] takes precedence over it.
// Returns an [Option].
func WithWireBytes(counter *atomic.Int64) Option {
	return func(c *config) {
		c.wireBytes = counter
	}
}

// WithRateLimitCounter sets a shared counter the rate governor increments
// once per rate-limited (HTTP 429) response observed on the wire, including
// each retried attempt, so a progress view can surface how throttled the run
// has been. It is raw telemetry, not a count of failed operations. A nil
// counter disables counting. Returns an [Option].
func WithRateLimitCounter(counter *atomic.Int64) Option {
	return func(c *config) {
		c.rateLimited = counter
	}
}

// WithGate sets the [Gate] every request routed through [Client.Do] takes a
// slot from before waiting on the rate governor, bounding how many requests
// are in flight at once across all workers. A nil gate leaves requests
// bounded only by the governor. Returns an [Option].
func WithGate(g Gate) Option {
	return func(c *config) {
		c.gate = g
	}
}

// WithLogger sets the logger every request is traced through when [New] builds
// its own HTTP client. Each attempt on the wire emits one [slog.LevelDebug]
// line carrying the method, URL, elapsed time to headers, and the status (or
// transport error) that attempt saw; the client retries server errors
// ([WithServerErrorRetry]) and rate-limited responses, so a retried request
// logs once per attempt. Downloads log their full signed artifact URL, so
// treat the debug output as sensitively as the token itself. A nil logger
// disables the logging, and a caller-supplied [WithHTTPClient] takes
// precedence over it. Returns an [Option].
func WithLogger(logger *slog.Logger) Option {
	return func(c *config) {
		c.logger = logger
	}
}

// WithHTTPClient sets the underlying [*http.Client]. A nil value leaves the
// client to build its own pooled default, whose transport bounds the time to
// first byte (see [WithResponseHeaderTimeout]); go-tfe's own pooled default is
// never used. Returns an [Option].
func WithHTTPClient(hc *http.Client) Option {
	return func(c *config) {
		c.httpClient = hc
	}
}

// New creates a new [Client].
//
// It constructs exactly one go-tfe client and one shared rate governor. It
// returns [ErrMissingToken] if no token was supplied via [WithToken].
func New(opts ...Option) (*Client, error) {
	cfg := newConfig(opts)

	if cfg.token == "" {
		return nil, fmt.Errorf("new client: %w", ErrMissingToken)
	}

	// All retry policy is deliberately owned by this package's bounded
	// [retryTransport] rather than go-tfe. With RetryServerErrors on, go-tfe
	// retries a 5xx 30 times under a linearly growing backoff (over six
	// minutes per request against a persistently failing endpoint, holding a
	// worker slot the whole time), none of it configurable through
	// [tfe.Config]; the flag stays off. Its 429 retry ignores the flag, so it
	// is starved instead: the transport never returns a 429 upward, retrying
	// within its own budget and converting exhaustion to an error go-tfe does
	// not retry.
	tc, err := tfe.NewClient(&tfe.Config{
		Address:           cfg.address,
		Token:             cfg.token,
		RetryServerErrors: false,
		HTTPClient:        resolveHTTPClient(&cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("new tfe client: %w", err)
	}

	return &Client{
		tfe:      tc,
		gate:     cfg.gate,
		governor: cfg.governor,
	}, nil
}

// newConfig resolves the default settings and applies each option over them.
func newConfig(opts []Option) config {
	cfg := config{
		address:               tfe.DefaultAddress,
		rateLimit:             DefaultRateLimit,
		runsListRateLimit:     DefaultRunsListRateLimit,
		responseHeaderTimeout: DefaultResponseHeaderTimeout,
		idleReadTimeout:       DefaultIdleReadTimeout,
		serverErrorRetries:    DefaultServerErrorRetries,
		serverErrorRetryDelay: DefaultServerErrorRetryDelay,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	// The governors are built once the options have settled the ceilings and
	// the counter, so the transport wiring below can bind the shared throttles.
	// Both increment the one rate-limited counter: it counts 429s observed on
	// the wire, whichever bucket they landed in.
	cfg.governor = NewGovernor(cfg.rateLimit, cfg.rateLimited)
	cfg.runsGovernor = NewGovernor(cfg.runsListRateLimit, cfg.rateLimited)

	return cfg
}

// resolveHTTPClient returns the caller-supplied HTTP client when one was set,
// or otherwise builds a pooled client whose transport bounds the time to first
// byte and each body read's idle wait. A caller-supplied [WithHTTPClient]
// client keeps its own transport and tuning, so neither bound nor the
// wire-byte counter applies to it; the governor and retry transports are the
// exception, wrapped around a copy, because their invariants are correctness
// bounds no request path may escape rather than instrumentation: every
// attempt pays the shared throttle, every response sheds its rate-limit
// header, and no 429 ever reaches go-tfe (whose unconditional 429 retry and
// fatal reset-header parse would otherwise survive on this path).
//
// Without one, go-tfe falls back to [cleanhttp.DefaultPooledClient], which sets
// no ResponseHeaderTimeout and leaves Client.Timeout zero, so one stalled
// connection wedges a worker forever. A non-zero Client.Timeout would cap a
// legitimately large streaming state, log, or tarball download, so the body is
// bounded per read instead: the idleTransport wrapper fails any read that sits
// past the idle-read timeout with no bytes arriving, closing the last "hang
// forever" hole while leaving slow-but-alive transfers uncapped.
//
// ResponseHeaderTimeout is an HTTP/1 transport setting the HTTP/2 round-tripper
// ignores, and DefaultPooledTransport force-attempts HTTP/2, which HCP Terraform
// negotiates over ALPN; left enabled, the header bound would be silently dead on
// the real connection. HTTP/2 is disabled so the timeout governs the time to
// first byte over HTTP/1, where multiplexing buys a single rate-limited host
// little. The idle-read bound relies on the same HTTP/1 semantics to unblock a
// stalled read by closing the connection.
func resolveHTTPClient(cfg *config) *http.Client {
	if cfg.httpClient != nil {
		hc := *cfg.httpClient

		next := hc.Transport
		if next == nil {
			next = http.DefaultTransport
		}

		hc.Transport = wrapRetryAndThrottle(next, cfg)

		return &hc
	}

	tr := cleanhttp.DefaultPooledTransport()
	tr.ForceAttemptHTTP2 = false
	tr.ResponseHeaderTimeout = cfg.responseHeaderTimeout
	tr.TLSHandshakeTimeout = DefaultTLSHandshakeTimeout

	// The archive workload splits across two hosts (the API and the artifact
	// store) with HTTP/2 disabled, so each concurrent worker holds its own HTTP/1
	// connection per host. The cleanhttp per-host idle cap of GOMAXPROCS+1 closes
	// most of those between artifact downloads, forcing a fresh dial and TLS
	// handshake per download under load; letting every pooled connection idle per
	// host keeps them reusable.
	tr.MaxIdleConnsPerHost = tr.MaxIdleConns

	var rt http.RoundTripper = &idleTransport{
		next:        tr,
		logger:      cfg.logger,
		idleTimeout: cfg.idleReadTimeout,
		wireBytes:   cfg.wireBytes,
	}

	return &http.Client{Transport: wrapRetryAndThrottle(rt, cfg)}
}

// wrapRetryAndThrottle stacks the two transports every request path must pass
// through around next: the governor transport below, so each physical attempt
// pays a rate token and reports its outcome, and the bounded retry above it,
// so every retried attempt re-enters the governor. The pair also holds the
// invariant that go-tfe never sees a 429 (the retry converts an unretried one
// to an error) and never sees the X-RateLimit-Limit header (the governor
// transport strips it), keeping go-tfe's own rate-limit machinery dormant on
// both the built and the caller-supplied client paths.
func wrapRetryAndThrottle(next http.RoundTripper, cfg *config) http.RoundTripper {
	return &retryTransport{
		next:             &throttleTransport{next: next, gov: cfg.governor, runsGov: cfg.runsGovernor},
		retries:          max(cfg.serverErrorRetries, 0),
		delay:            cfg.serverErrorRetryDelay,
		rateLimitRetries: DefaultRateLimitRetries,
	}
}

// TFE returns the underlying go-tfe client so collectors can build closures
// over its many typed services. Requests made directly on the returned client
// bypass the gate but not the governor, which lives in the client's HTTP
// transport; route them through [Client.Do] to stay bounded.
func (c *Client) TFE() *tfe.Client {
	return c.tfe
}

// RateStatus reports the general rate governor's current adaptive rate in
// requests per second and how much of a rate-limit cooldown pause remains
// (zero while launches are flowing), so a progress view can show the run
// adapting to the server's capacity. The runs-list governor is not reflected
// here: its rate is a near-constant trickle by design, so the general governor
// is the one whose movement means something.
func (c *Client) RateStatus() (float64, time.Duration) {
	return c.governor.Snapshot()
}

// Do runs fn after taking a slot from the optional [Gate], passing the
// underlying go-tfe client. It is the single chokepoint every API call should
// pass through so the whole run shares one parallelism bound; the slot is
// held until fn returns, so buffered downloads count against the bound for
// their whole transfer. Rate limiting is not applied here but at the HTTP
// transport, where every attempt the call causes pays its own token; taking
// the gate first still means a queued request starts no attempt it cannot use
// yet. A ctx already done returns its error without invoking fn. The error
// from fn is returned unmodified so callers can classify it with [Classify],
// [IsTransient], [IsTerminal], or [IsForbidden].
func (c *Client) Do(ctx context.Context, fn func(context.Context, *tfe.Client) error) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("request context: %w", err)
	}

	if c.gate != nil {
		err := c.gate.Acquire(ctx)
		if err != nil {
			return fmt.Errorf("gate acquire: %w", err)
		}

		defer c.gate.Release()
	}

	return fn(ctx, c.tfe)
}

// ErrPaginationStalled reports a pagination whose NextPage did not advance past
// the current page while still claiming further pages exist: a misbehaving
// server or a cycle. The pages past the stall were never fetched, so the
// listing is incomplete and must not be treated as a complete enumeration.
var ErrPaginationStalled = errors.New("pagination stalled")

// pageFetch is the page-read closure [Paginate] walks: one list call returning
// the page's items and the server's pagination for it.
type pageFetch[T any] func(context.Context, *tfe.Client, tfe.ListOptions) ([]T, *tfe.Pagination, error)

// Paginate walks a paginated list endpoint through [Client.Do], accumulating
// every page's items into one slice in page order.
//
// Every page is requested at [MaxPageSize] to minimize round-trips. The first
// page is fetched alone, since its pagination reveals the listing's extent;
// the remaining pages are then fetched concurrently, capped at
// [DefaultPageConcurrency] and by the client's gate like every request, and
// stitched in page order. A response that carries no extent (a nil pagination
// or zero TotalPages) falls back to following NextPage serially. A listing
// that grows while the batch is in flight advertises a further next page on
// its last page, and the walk continues serially from there, so the tail is
// never silently dropped. Paging through a list that is changing mid-walk can
// duplicate or miss items whether or not the pages load concurrently; the
// shorter window narrows that skew.
//
// A pagination that stalls (a NextPage that claims more pages but does not
// advance) returns [ErrPaginationStalled] rather than the partial listing:
// the callers of a complete enumeration settle absences and mark surfaces
// complete from it, so a silently truncated list would convert the unreached
// tail into recorded absences.
func Paginate[T any](ctx context.Context, c *Client, fetch pageFetch[T]) ([]T, error) {
	first, pg, err := fetchPage(ctx, c, fetch, 1)
	if err != nil {
		return nil, err
	}

	if pg == nil || pg.NextPage == 0 {
		return first, nil
	}

	if pg.TotalPages <= 1 {
		return paginateFrom(ctx, c, fetch, first, 1, pg.NextPage)
	}

	// The extent is known: fetch the remaining pages concurrently into
	// per-page slots, so the goroutines share nothing and the stitched result
	// keeps the listing's own order.
	pages := make([][]T, pg.TotalPages+1)
	pages[1] = first

	lastPage := pg.TotalPages

	var lastPg *tfe.Pagination

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(DefaultPageConcurrency)

	for p := 2; p <= lastPage; p++ {
		g.Go(func() error {
			items, pgN, ferr := fetchPage(gctx, c, fetch, p)
			if ferr != nil {
				return ferr
			}

			pages[p] = items

			// Only the final page can reveal growth past the batch; exactly one
			// goroutine writes this, and the group's Wait orders the read below.
			if p == lastPage {
				lastPg = pgN
			}

			return nil
		})
	}

	err = g.Wait()
	if err != nil {
		return nil, err //nolint:wrapcheck // Page fetch errors pass through unmodified for Classify.
	}

	var all []T

	for _, page := range pages {
		all = append(all, page...)
	}

	// A listing that grew while the batch was in flight advertises a further
	// page on the last one; walk the tail serially so it is not dropped.
	if lastPg != nil && lastPg.NextPage != 0 {
		return paginateFrom(ctx, c, fetch, all, lastPage, lastPg.NextPage)
	}

	return all, nil
}

// fetchPage reads one page through [Client.Do] at the maximum page size.
func fetchPage[T any](ctx context.Context, c *Client, fetch pageFetch[T], page int) ([]T, *tfe.Pagination, error) {
	var (
		items []T
		pg    *tfe.Pagination
	)

	err := c.Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		var e error

		items, pg, e = fetch(ctx, tc, tfe.ListOptions{PageNumber: page, PageSize: MaxPageSize})

		return e
	})
	if err != nil {
		return nil, nil, err
	}

	return items, pg, nil
}

// paginateFrom follows NextPage serially from the page numbered next, appending
// each page's items to all. The prev argument is the page that advertised next.
//
// The server drives this walk through NextPage, and a well-formed response
// always advances it. A NextPage that does not move past its own page would
// otherwise loop forever, re-fetching the same page and growing the result
// without bound; the stall surfaces as [ErrPaginationStalled] rather than a
// truncated listing the caller would mistake for a complete one.
func paginateFrom[T any](
	ctx context.Context,
	c *Client,
	fetch pageFetch[T],
	all []T,
	prev, next int,
) ([]T, error) {
	for {
		if next <= prev {
			return nil, fmt.Errorf("%w: page %d reports next page %d", ErrPaginationStalled, prev, next)
		}

		items, pg, err := fetchPage(ctx, c, fetch, next)
		if err != nil {
			return nil, err
		}

		all = append(all, items...)

		if pg == nil || pg.NextPage == 0 {
			return all, nil
		}

		prev, next = next, pg.NextPage
	}
}

// OpenState opens a state version blob at its signed download URL for streaming
// to disk, returning the live response body the caller must close. The URL is
// the raw or JSON-format download URL carried on a [tfe.StateVersion]. It
// replaces the SDK's buffering [tfe.StateVersions.Download], releasing the
// request gate at the response headers rather than holding it for the whole
// transfer, so peak memory stays bounded regardless of the blob's size.
func (c *Client) OpenState(ctx context.Context, downloadURL string) (io.ReadCloser, error) {
	req, err := c.tfe.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new state request: %w", err)
	}

	// Match the SDK's Download, whose signed URL is served as application/json.
	req.Header.Set("Accept", "application/json")

	return c.openRaw(ctx, "download state version", req)
}

// OpenConfigurationVersion opens a configuration version tarball (tar.gz) by its
// id for streaming to disk, returning the live response body the caller must
// close. It replaces the SDK's buffering [tfe.ConfigurationVersions.Download].
func (c *Client) OpenConfigurationVersion(ctx context.Context, cvID string) (io.ReadCloser, error) {
	u := fmt.Sprintf("configuration-versions/%s/download", url.PathEscape(cvID))

	req, err := c.tfe.NewRequest("GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("new configuration version request: %w", err)
	}

	return c.openRaw(ctx, "download configuration version", req)
}

// OpenPlanJSON opens a plan's structured JSON output (the plans/{id}/json-output
// artifact) by its id for streaming to disk, returning the live response body
// the caller must close. It replaces the SDK's buffering
// [tfe.Plans.ReadJSONOutput]. A run that produced no JSON plan answers 204 No
// Content, surfaced as an empty body the archive path records as a settled gap.
func (c *Client) OpenPlanJSON(ctx context.Context, planID string) (io.ReadCloser, error) {
	u := fmt.Sprintf("plans/%s/json-output", url.PathEscape(planID))

	req, err := c.tfe.NewRequest("GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("new plan JSON request: %w", err)
	}

	return c.openRaw(ctx, "download plan JSON", req)
}

// OpenPlanLog opens a plan's full log by its id for streaming to disk, returning
// the live response body the caller must close.
//
// The plan is read first so its signed log URL is fresh, then the whole log is
// opened in one request. The chunked [tfe.LogReader] behind the SDK's Logs
// methods exists to tail a live run: it consumes only the first few kilobytes of
// each response before issuing another request, which makes a large finished log
// take minutes. An archived run is terminal, so its log is complete and one
// streamed read suffices; the stream's STX/ETX framing is trimmed on the fly by
// a [logReader]. Returns an error wrapping [ErrMissingLogURL] when the plan
// carries no log URL.
func (c *Client) OpenPlanLog(ctx context.Context, planID string) (io.ReadCloser, error) {
	var logURL string

	err := c.Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		p, e := tc.Plans.Read(ctx, planID)
		if e != nil {
			return fmt.Errorf("read plan: %w", e)
		}

		logURL = p.LogReadURL

		return nil
	})
	if err != nil {
		return nil, err
	}

	r, err := c.openLog(ctx, logURL)
	if err != nil {
		return nil, fmt.Errorf("open plan %s log: %w", planID, err)
	}

	return r, nil
}

// OpenApplyLog opens an apply's full log by its id for streaming to disk, with
// the same single-request semantics as [Client.OpenPlanLog]. Returns an error
// wrapping [ErrMissingLogURL] when the apply carries no log URL.
func (c *Client) OpenApplyLog(ctx context.Context, applyID string) (io.ReadCloser, error) {
	var logURL string

	err := c.Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		a, e := tc.Applies.Read(ctx, applyID)
		if e != nil {
			return fmt.Errorf("read apply: %w", e)
		}

		logURL = a.LogReadURL

		return nil
	})
	if err != nil {
		return nil, err
	}

	r, err := c.openLog(ctx, logURL)
	if err != nil {
		return nil, fmt.Errorf("open apply %s log: %w", applyID, err)
	}

	return r, nil
}

// openLog opens the whole log at logURL for streaming through the shared client,
// wrapping the body in a [logReader] that trims the STX/ETX framing on the fly.
// A missing URL is [ErrMissingLogURL]; a nil body (a 304) passes through as a
// nil reader for the archive path to record as a settled gap.
func (c *Client) openLog(ctx context.Context, logURL string) (io.ReadCloser, error) {
	if logURL == "" {
		return nil, ErrMissingLogURL
	}

	req, err := c.tfe.NewRequest("GET", logURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new log request: %w", err)
	}

	// The SDK stamps every GET with the JSON:API accept header, but the signed
	// URL is served by archivist, which answers 400 Bad Request to any request
	// that does not accept text/plain. Its own log reader sends no accept header
	// at all; match that permissively.
	req.Header.Set("Accept", "text/plain, */*")

	body, err := c.openRaw(ctx, "get log", req)
	if err != nil {
		return nil, err
	}

	return newLogReader(body), nil
}

// openRaw sends the prepared GET through the shared client and returns the live
// response body from [tfe.ClientRequest.DoRaw]. It suits large artifacts that
// stream to disk: the gate slot [Client.Do] holds is released when this returns
// at the response headers, so the body transfers without occupying it.
//
// DoRaw skips the SDK's response-code check, surfacing a non-2xx only as a
// generic "error HTTP response: N". The status is captured through a response
// header hook and translated by [statusError] so [Classify] yields the same
// [Kind] the SDK's checked request path yields (a 404 stays [KindTerminal], a
// 403 [KindForbidden]). A 304 yields a nil body.
func (c *Client) openRaw(ctx context.Context, action string, req *tfe.ClientRequest) (io.ReadCloser, error) {
	var body io.ReadCloser

	err := c.Do(ctx, func(ctx context.Context, _ *tfe.Client) error {
		var status int

		ctx = tfe.ContextWithResponseHeaderHook(ctx, func(s int, _ http.Header) {
			status = s
		})

		rc, e := req.DoRaw(ctx)
		if e != nil {
			return fmt.Errorf("%s: %w", action, statusError(status, e))
		}

		body = rc

		return nil
	})
	if err != nil {
		return nil, err
	}

	return body, nil
}

// logReader trims the STX/ETX control characters framing a completed log stream
// as it flows, so a directly streamed log stays byte-identical to one archived
// through the SDK's chunked [tfe.LogReader], which strips the framing while
// chunking. A leading STX is dropped, and a trailing ETX is dropped only when
// that leading STX was present, so a stream that never adopted the framing (a
// bare interior or trailing ETX with no leading STX) passes through untouched.
//
// It peeks one byte past a candidate trailing ETX to tell a real end-of-stream
// marker from an interior one, so nothing but the framing bytes is dropped,
// without buffering the whole log. Create instances with [newLogReader].
type logReader struct {
	rc      io.ReadCloser
	br      *bufio.Reader
	started bool
	framed  bool
}

// newLogReader wraps rc in a [logReader]. A nil rc (a 304 with no body) yields a
// nil reader, matching the streaming archive path's empty-artifact handling.
func newLogReader(rc io.ReadCloser) io.ReadCloser {
	if rc == nil {
		return nil
	}

	return &logReader{rc: rc, br: bufio.NewReader(rc)}
}

// Read fills p with the next trimmed log bytes.
func (l *logReader) Read(p []byte) (int, error) {
	n := 0

	for n < len(p) {
		b, err := l.next()
		if err != nil {
			return n, err
		}

		p[n] = b
		n++
	}

	return n, nil
}

// next returns the next output byte, or [io.EOF] once the trimmed stream is
// spent. It drops a leading STX on the first call, and drops an ETX only when it
// is both framed and the final byte, peeking one byte ahead to tell a trailing
// ETX from an interior one.
func (l *logReader) next() (byte, error) {
	if !l.started {
		err := l.start()
		if err != nil {
			return 0, err
		}
	}

	b, err := l.readByte()
	if err != nil {
		return 0, err
	}

	// A framed stream's trailing ETX is dropped; an interior ETX (more bytes
	// still follow) is kept. A peek that ends the stream (io.EOF) marks the ETX as
	// trailing and drops it; any other peek error means the trailing-vs-interior
	// call cannot be made, so surface it rather than emit a framing byte the SDK
	// path would have stripped (and defer the error one byte).
	if l.framed && b == 0x03 {
		_, peekErr := l.br.Peek(1)

		switch {
		case errors.Is(peekErr, io.EOF):
			return 0, io.EOF
		case peekErr != nil:
			return 0, peekErr //nolint:wrapcheck // A transparent reader wrapper.
		}
	}

	return b, nil
}

// start consumes the first byte, recording whether the stream is STX-framed and
// putting a non-STX first byte back for the normal read path.
func (l *logReader) start() error {
	l.started = true

	b, err := l.readByte()
	if err != nil {
		return err
	}

	// A leading STX marks a framed stream and is dropped; any other first byte
	// is real content, so put it back for the read below.
	if b == 0x02 {
		l.framed = true

		return nil
	}

	err = l.br.UnreadByte()
	if err != nil {
		return err //nolint:wrapcheck // A transparent reader wrapper.
	}

	return nil
}

// readByte reads one raw byte from the underlying log body.
func (l *logReader) readByte() (byte, error) {
	b, err := l.br.ReadByte()

	return b, err //nolint:wrapcheck // A transparent reader wrapper.
}

// Close closes the underlying log body.
func (l *logReader) Close() error {
	return l.rc.Close() //nolint:wrapcheck // A transparent reader wrapper.
}
