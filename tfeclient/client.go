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
