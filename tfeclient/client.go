package tfeclient

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-tfe"
	"golang.org/x/time/rate"
)

// Default rate-limit settings for the shared limiter.
const (
	// DefaultRateLimit is the default sustained request rate, in requests per
	// second, applied across every worker sharing a [Client].
	DefaultRateLimit rate.Limit = 30

	// DefaultBurst is the default token-bucket burst size for the shared
	// limiter.
	DefaultBurst = 30
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
// until a slot is free or ctx is done and Release returns the slot, so a
// resizable implementation can scale the client's effective parallelism while
// a run is live. See [go.jacobcolvin.com/hcp_archiver/workpool.Pool] for an
// implementation.
type Gate interface {
	// Acquire takes a slot, blocking until one is free or ctx is done.
	Acquire(ctx context.Context) error
	// Release returns a slot taken by Acquire.
	Release()
}

// Client is the single, worker-safe point of contact with HCP Terraform.
//
// It holds exactly one underlying go-tfe client and one shared
// [rate.Limiter], so that N concurrent workers paginating and downloading
// share a single aggregate throttle rather than each retrying in isolation.
// Every request routed through [Client.Do] first takes a slot from the
// optional [Gate], then waits on that limiter, so a caller can bound and
// re-bound the whole run's parallelism in one place.
//
// Create instances with [New]. A Client is safe for concurrent use.
type Client struct {
	tfe     *tfe.Client
	limiter *rate.Limiter
	gate    Gate
}

// config holds the resolved settings a [Client] is built from.
type config struct {
	httpClient            *http.Client
	logger                *slog.Logger
	wireBytes             *atomic.Int64
	rateLimited           *atomic.Int64
	gate                  Gate
	address               string
	token                 string
	limit                 rate.Limit
	burst                 int
	responseHeaderTimeout time.Duration
	idleReadTimeout       time.Duration
}

// Option configures a [Client] during [New].
//
// Options of this type:
//   - [WithToken]
//   - [WithAddress]
//   - [WithRateLimit]
//   - [WithResponseHeaderTimeout]
//   - [WithIdleReadTimeout]
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

// WithRateLimit sets the shared limiter's sustained rate, in requests per
// second, and burst size. Non-positive values keep the defaults.
// Returns an [Option].
func WithRateLimit(perSecond float64, burst int) Option {
	return func(c *config) {
		if perSecond > 0 {
			c.limit = rate.Limit(perSecond)
		}

		if burst > 0 {
			c.burst = burst
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

// WithWireBytes sets a shared counter that accumulates every response-body
// byte as it is read off the wire when [New] builds its own HTTP client, so a
// progress view can derive live throughput while a large transfer is still in
// flight. A nil counter disables counting, and a caller-supplied
// [WithHTTPClient] takes precedence over it. Returns an [Option].
func WithWireBytes(counter *atomic.Int64) Option {
	return func(c *config) {
		c.wireBytes = counter
	}
}

// WithRateLimitCounter sets a shared counter that increments once per
// rate-limited (HTTP 429) response observed on the wire when [New] builds its
// own HTTP client. The go-tfe client retries a 429 internally after backing
// off, so each retried attempt counts too; the counter is the raw pressure
// signal an adaptive scaler samples, not a count of failed operations. A nil
// counter disables counting, and a caller-supplied [WithHTTPClient] takes
// precedence over it. Returns an [Option].
func WithRateLimitCounter(counter *atomic.Int64) Option {
	return func(c *config) {
		c.rateLimited = counter
	}
}

// WithGate sets the [Gate] every request routed through [Client.Do] takes a
// slot from before waiting on the rate limiter, bounding how many requests
// are in flight at once across all workers. A nil gate leaves requests
// bounded only by the limiter. Returns an [Option].
func WithGate(g Gate) Option {
	return func(c *config) {
		c.gate = g
	}
}

// WithLogger sets the logger every request is traced through when [New] builds
// its own HTTP client. Each attempt on the wire emits one [slog.LevelDebug]
// line carrying the method, URL, elapsed time to headers, and the status (or
// transport error) that attempt saw; the go-tfe client retries rate-limited
// and server-error responses internally, so a retried request logs once per
// attempt. Downloads log their full signed artifact URL, so treat the debug
// output as sensitively as the token itself. A nil logger disables the
// logging, and a caller-supplied [WithHTTPClient] takes precedence over it.
// Returns an [Option].
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
// It constructs exactly one go-tfe client (with server-error retry enabled)
// and one shared [rate.Limiter]. It returns [ErrMissingToken] if no token was
// supplied via [WithToken].
func New(opts ...Option) (*Client, error) {
	cfg := newConfig(opts)

	if cfg.token == "" {
		return nil, fmt.Errorf("new client: %w", ErrMissingToken)
	}

	tc, err := tfe.NewClient(&tfe.Config{
		Address:           cfg.address,
		Token:             cfg.token,
		RetryServerErrors: true,
		HTTPClient:        resolveHTTPClient(&cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("new tfe client: %w", err)
	}

	return &Client{
		tfe:     tc,
		limiter: rate.NewLimiter(cfg.limit, cfg.burst),
		gate:    cfg.gate,
	}, nil
}

// newConfig resolves the default settings and applies each option over them.
func newConfig(opts []Option) config {
	cfg := config{
		address:               tfe.DefaultAddress,
		limit:                 DefaultRateLimit,
		burst:                 DefaultBurst,
		responseHeaderTimeout: DefaultResponseHeaderTimeout,
		idleReadTimeout:       DefaultIdleReadTimeout,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	return cfg
}

// resolveHTTPClient returns the caller-supplied HTTP client when one was set,
// or otherwise builds a pooled client whose transport bounds the time to first
// byte and each body read's idle wait. A caller-supplied [WithHTTPClient]
// client is returned as-is, so neither bound nor the wire-byte counter applies
// to it.
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
		return cfg.httpClient
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

	return &http.Client{Transport: &idleTransport{
		next:        tr,
		logger:      cfg.logger,
		idleTimeout: cfg.idleReadTimeout,
		wireBytes:   cfg.wireBytes,
		rateLimited: cfg.rateLimited,
	}}
}

// TFE returns the underlying go-tfe client so collectors can build closures
// over its many typed services. Requests made directly on the returned client
// bypass the shared limiter; route them through [Client.Do] to stay throttled.
func (c *Client) TFE() *tfe.Client {
	return c.tfe
}

// Do runs fn after taking a slot from the optional [Gate] and waiting on the
// shared limiter, passing the underlying go-tfe client. It is the single
// chokepoint every request should pass through so the whole run shares one
// aggregate throttle and one parallelism bound. The gate is taken before the
// limiter so a queued request does not burn a rate token it cannot use yet,
// and the slot is held until fn returns, so buffered downloads count against
// the bound for their whole transfer. The error from fn is returned
// unmodified so callers can classify it with [Classify], [IsTransient],
// [IsTerminal], or [IsForbidden].
func (c *Client) Do(ctx context.Context, fn func(context.Context, *tfe.Client) error) error {
	if c.gate != nil {
		err := c.gate.Acquire(ctx)
		if err != nil {
			return fmt.Errorf("gate acquire: %w", err)
		}

		defer c.gate.Release()
	}

	err := c.limiter.Wait(ctx)
	if err != nil {
		return fmt.Errorf("rate limiter wait: %w", err)
	}

	return fn(ctx, c.tfe)
}

// Paginate walks a paginated list endpoint through [Client.Do], accumulating
// every page's items into one slice.
//
// It advances the page number starting at 1, invoking fetch once per page with
// the [tfe.ListOptions] to apply, and stops when the returned
// [*tfe.Pagination] reports no next page. Each page fetch passes through the
// shared limiter.
func Paginate[T any](
	ctx context.Context,
	c *Client,
	fetch func(context.Context, *tfe.Client, tfe.ListOptions) ([]T, *tfe.Pagination, error),
) ([]T, error) {
	var all []T

	page := 1

	for {
		var (
			items []T
			pg    *tfe.Pagination
		)

		err := c.Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
			var e error

			items, pg, e = fetch(ctx, tc, tfe.ListOptions{PageNumber: page})

			return e
		})
		if err != nil {
			return nil, err
		}

		all = append(all, items...)

		if pg == nil || pg.NextPage == 0 {
			break
		}

		// The server drives pagination through NextPage, and a well-formed response
		// always advances it. A NextPage that does not move past the current page (a
		// misbehaving server, a cycle) would otherwise loop forever, re-fetching the
		// same page and growing all without bound; stop with what was gathered.
		if pg.NextPage <= page {
			break
		}

		page = pg.NextPage
	}

	return all, nil
}

// DownloadState downloads a state version blob from its signed download URL
// (the raw or JSON-format URL carried on a [tfe.StateVersion]). The go-tfe
// method buffers the whole blob, so this returns bytes.
func (c *Client) DownloadState(ctx context.Context, downloadURL string) ([]byte, error) {
	var data []byte

	err := c.Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		var e error

		data, e = tc.StateVersions.Download(ctx, downloadURL)
		if e != nil {
			return fmt.Errorf("download state version: %w", e)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return data, nil
}

// DownloadConfigurationVersion downloads a configuration version tarball
// (tar.gz) by its id. The go-tfe method buffers the whole tarball, so this
// returns bytes.
func (c *Client) DownloadConfigurationVersion(ctx context.Context, cvID string) ([]byte, error) {
	var data []byte

	err := c.Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		var e error

		data, e = tc.ConfigurationVersions.Download(ctx, cvID)
		if e != nil {
			return fmt.Errorf("download configuration version: %w", e)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return data, nil
}

// DownloadPlanLog downloads a plan's full log by its id.
//
// The plan is read first so its signed log URL is fresh, then the whole log
// object is fetched in one request. The chunked [tfe.LogReader] behind the
// SDK's Logs methods exists to tail a live run: it consumes only the first few
// kilobytes of each response before issuing another request, which makes a
// large finished log take minutes. An archived run is terminal, so its log is
// complete and one read suffices. Returns an error wrapping [ErrMissingLogURL]
// when the plan carries no log URL.
func (c *Client) DownloadPlanLog(ctx context.Context, planID string) ([]byte, error) {
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

	data, err := c.downloadLog(ctx, logURL)
	if err != nil {
		return nil, fmt.Errorf("download plan %s log: %w", planID, err)
	}

	return data, nil
}

// DownloadApplyLog downloads an apply's full log by its id, with the same
// single-request semantics as [Client.DownloadPlanLog]. Returns an error
// wrapping [ErrMissingLogURL] when the apply carries no log URL.
func (c *Client) DownloadApplyLog(ctx context.Context, applyID string) ([]byte, error) {
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

	data, err := c.downloadLog(ctx, logURL)
	if err != nil {
		return nil, fmt.Errorf("download apply %s log: %w", applyID, err)
	}

	return data, nil
}

// downloadLog fetches the whole log object at logURL in one request through
// the shared limiter, using the go-tfe request machinery so retries and error
// classification match every other request.
func (c *Client) downloadLog(ctx context.Context, logURL string) ([]byte, error) {
	if logURL == "" {
		return nil, ErrMissingLogURL
	}

	var data []byte

	err := c.Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		req, e := tc.NewRequest("GET", logURL, nil)
		if e != nil {
			return fmt.Errorf("new log request: %w", e)
		}

		// The SDK stamps every GET with the JSON:API accept header, but the
		// signed URL is served by archivist, which answers 400 Bad Request to
		// any request that does not accept text/plain. Its own log reader
		// sends no accept header at all; match that permissively.
		req.Header.Set("Accept", "text/plain, */*")

		var buf bytes.Buffer

		e = req.Do(ctx, &buf)
		if e != nil {
			return fmt.Errorf("get log: %w", e)
		}

		data = trimLogMarkers(buf.Bytes())

		return nil
	})
	if err != nil {
		return nil, err
	}

	return data, nil
}

// trimLogMarkers strips the STX/ETX control characters framing a completed
// log stream. [tfe.LogReader] removes them while chunking, so trimming keeps
// a directly downloaded log byte-identical to one archived through it. The
// ETX is only trimmed behind an STX, mirroring the reader, so a stream that
// never adopted the framing passes through untouched.
func trimLogMarkers(b []byte) []byte {
	if len(b) == 0 || b[0] != 0x02 {
		return b
	}

	b = b[1:]

	if n := len(b); n > 0 && b[n-1] == 0x03 {
		b = b[:n-1]
	}

	return b
}
