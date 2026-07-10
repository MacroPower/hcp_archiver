package tfeclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
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

// Client is the single, worker-safe point of contact with HCP Terraform.
//
// It holds exactly one underlying go-tfe client and one shared
// [rate.Limiter], so that N concurrent workers paginating and downloading
// share a single aggregate throttle rather than each retrying in isolation.
// Every request routed through [Client.Do] first waits on that limiter.
//
// Create instances with [New]. A Client is safe for concurrent use.
type Client struct {
	tfe     *tfe.Client
	limiter *rate.Limiter
}

// config holds the resolved settings a [Client] is built from.
type config struct {
	httpClient            *http.Client
	address               string
	token                 string
	limit                 rate.Limit
	burst                 int
	responseHeaderTimeout time.Duration
}

// Option configures a [Client] during [New].
//
// Options of this type:
//   - [WithToken]
//   - [WithAddress]
//   - [WithRateLimit]
//   - [WithResponseHeaderTimeout]
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
// runs uncapped. A non-positive value keeps [DefaultResponseHeaderTimeout], and
// a caller-supplied [WithHTTPClient] takes precedence over it. Returns an
// [Option].
func WithResponseHeaderTimeout(timeout time.Duration) Option {
	return func(c *config) {
		if timeout > 0 {
			c.responseHeaderTimeout = timeout
		}
	}
}

// WithHTTPClient sets the underlying [*http.Client]. A nil value keeps
// go-tfe's pooled default. Returns an [Option].
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
	}, nil
}

// newConfig resolves the default settings and applies each option over them.
func newConfig(opts []Option) config {
	cfg := config{
		address:               tfe.DefaultAddress,
		limit:                 DefaultRateLimit,
		burst:                 DefaultBurst,
		responseHeaderTimeout: DefaultResponseHeaderTimeout,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	return cfg
}

// resolveHTTPClient returns the caller-supplied HTTP client when one was set, or
// otherwise builds a pooled client whose transport bounds the time to first
// byte.
//
// Without one, go-tfe falls back to [cleanhttp.DefaultPooledClient], which sets
// no ResponseHeaderTimeout and leaves Client.Timeout zero, so one stalled
// connection wedges a worker forever. The body transfer stays unbounded (a
// non-zero Client.Timeout would cap a legitimately large streaming state, log,
// or tarball download), so this narrows "hang forever" to the header wait
// rather than eliminating a mid-body stall.
func resolveHTTPClient(cfg *config) *http.Client {
	if cfg.httpClient != nil {
		return cfg.httpClient
	}

	tr := cleanhttp.DefaultPooledTransport()
	tr.ResponseHeaderTimeout = cfg.responseHeaderTimeout

	return &http.Client{Transport: tr}
}

// TFE returns the underlying go-tfe client so collectors can build closures
// over its many typed services. Requests made directly on the returned client
// bypass the shared limiter; route them through [Client.Do] to stay throttled.
func (c *Client) TFE() *tfe.Client {
	return c.tfe
}

// Do runs fn after waiting on the shared limiter, passing the underlying
// go-tfe client. It is the single gate every request should pass through so
// the whole run shares one aggregate throttle. The error from fn is returned
// unmodified so callers can classify it with [Classify], [IsTransient],
// [IsTerminal], or [IsForbidden].
func (c *Client) Do(ctx context.Context, fn func(context.Context, *tfe.Client) error) error {
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

// PlanLogs opens the logs of a plan by its id. The returned reader streams the
// log and performs its own network I/O as it is read, so those reads are not
// bounded by the shared limiter.
func (c *Client) PlanLogs(ctx context.Context, planID string) (io.Reader, error) {
	var r io.Reader

	err := c.Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		var e error

		r, e = tc.Plans.Logs(ctx, planID)
		if e != nil {
			return fmt.Errorf("read plan logs: %w", e)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return r, nil
}

// ApplyLogs opens the logs of an apply by its id. The returned reader streams
// the log and performs its own network I/O as it is read, so those reads are
// not bounded by the shared limiter.
func (c *Client) ApplyLogs(ctx context.Context, applyID string) (io.Reader, error) {
	var r io.Reader

	err := c.Do(ctx, func(ctx context.Context, tc *tfe.Client) error {
		var e error

		r, e = tc.Applies.Logs(ctx, applyID)
		if e != nil {
			return fmt.Errorf("read apply logs: %w", e)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return r, nil
}
