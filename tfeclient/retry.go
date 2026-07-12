package tfeclient

import (
	"context"
	"io"
	"net/http"
	"time"
)

// Default in-client retry settings for server errors and transport failures.
const (
	// DefaultServerErrorRetries is the default number of additional attempts
	// the client makes after a request fails at the transport or answers with
	// a server error (5xx).
	DefaultServerErrorRetries = 3

	// DefaultServerErrorRetryDelay is the default wait before the first
	// server-error retry; each subsequent retry doubles it, capped at
	// maxServerErrorRetryDelay.
	DefaultServerErrorRetryDelay = 1 * time.Second

	// The maxServerErrorRetryDelay cap bounds the doubling retry backoff so a
	// generously configured retry count cannot stretch one request's stall into
	// minutes.
	maxServerErrorRetryDelay = 5 * time.Second

	// The drainLimit bound caps how much of a failed attempt's body is read
	// before the retry, enough to let the connection be reused without
	// buffering a large error page.
	drainLimit = 4 << 10
)

// retryTransport retries idempotent requests that failed at the transport or
// answered with a server error, with a bounded, doubling backoff.
//
// It exists because the retry go-tfe ships is unbounded in practice: with
// RetryServerErrors enabled, a 5xx is retried 30 times under a linearly
// growing jitter backoff (over six minutes for a persistently failing
// endpoint), none of it configurable through [tfe.Config]. That stall holds a
// worker slot the whole time, multiplies under the collect layer's own blob
// retries, and is invisible to the pool controller, which reacts only to rate
// limiting. The client therefore disables go-tfe's server-error retry and owns
// the policy here, at the transport, where the status code still exists (the
// go-tfe error rendering discards it).
//
// Only a request with no body (every read this client makes) is retried, so a
// consumed body is never re-sent. A 429 is never retried here: it passes
// through to the go-tfe client's rate-limit handling, which honors the
// server's reset time and feeds the rate-limited counter. A context
// cancellation ends the retries immediately.
type retryTransport struct {
	next    http.RoundTripper
	delay   time.Duration
	retries int
}

// RoundTrip delegates to the wrapped transport, retrying a retryable failed
// attempt up to the configured count. The final attempt's response or error is
// returned as-is, so the layers above see exactly what an unretried request
// would have produced.
func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	delay := t.delay

	for attempt := 0; ; attempt++ {
		resp, err := t.next.RoundTrip(req)
		if attempt >= t.retries || !retryableAttempt(req, resp, err) {
			return resp, err //nolint:wrapcheck // A transparent transport wrapper.
		}

		// Drain a little of the failed attempt's body and close it so the
		// connection can be reused for the retry.
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit)) //nolint:errcheck // Best-effort drain.
			_ = resp.Body.Close()                                             //nolint:errcheck // Best-effort close.
		}

		werr := waitRetry(req.Context(), delay)
		if werr != nil {
			if err != nil {
				return nil, err //nolint:wrapcheck // A transparent transport wrapper.
			}

			return nil, werr
		}

		delay = min(delay*2, maxServerErrorRetryDelay)
	}
}

// retryableAttempt reports whether one attempt's outcome should be retried: a
// transport-level failure or a server error (5xx), on a request that is safe
// to re-send. A request whose context is already done is never retried, so a
// cancellation is not mistaken for a server fault.
func retryableAttempt(req *http.Request, resp *http.Response, err error) bool {
	// Only a body-less request is safely re-sendable; a consumed body would
	// re-send empty. Every request this archiver makes is a body-less read.
	if req.Body != nil && req.Body != http.NoBody {
		return false
	}

	if req.Context().Err() != nil {
		return false
	}

	if err != nil {
		return true
	}

	return resp.StatusCode >= http.StatusInternalServerError
}

// waitRetry sleeps d or until ctx is done, whichever comes first, returning
// the context's error when it ended the wait. A non-positive d only checks the
// context.
func waitRetry(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err() //nolint:wrapcheck // A transparent transport helper.
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // A transparent transport helper.
	case <-timer.C:
		return nil
	}
}
