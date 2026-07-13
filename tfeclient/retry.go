package tfeclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Default in-client retry settings for server errors, transport failures, and
// rate-limited responses.
const (
	// DefaultServerErrorRetries is the default number of additional attempts
	// the client makes after a request fails at the transport or answers with
	// a server error (5xx).
	DefaultServerErrorRetries = 3

	// DefaultServerErrorRetryDelay is the default wait before the first
	// server-error retry; each subsequent retry doubles it, capped at
	// maxServerErrorRetryDelay.
	DefaultServerErrorRetryDelay = 1 * time.Second

	// DefaultRateLimitRetries is the number of additional attempts the client
	// makes after a request answers 429. A rate-limit retry needs no backoff
	// of its own: re-entering the transport blocks in the shared [Governor]
	// until the server's advertised reset has passed and pays a fresh token
	// at the freshly lowered rate.
	DefaultRateLimitRetries = 5

	// The maxServerErrorRetryDelay cap bounds the doubling retry backoff so a
	// generously configured retry count cannot stretch one request's stall into
	// minutes.
	maxServerErrorRetryDelay = 5 * time.Second

	// The drainLimit bound caps how much of a failed attempt's body is read
	// before the retry, enough to let the connection be reused without
	// buffering a large error page.
	drainLimit = 4 << 10
)

// retryTransport retries idempotent requests that failed at the transport,
// answered with a server error, or were rate limited, each under its own
// bounded budget in one loop so nested retry layers can never multiply.
//
// It exists because the retry go-tfe ships is unusable here. With
// RetryServerErrors enabled, a 5xx is retried 30 times under a linearly
// growing jitter backoff (over six minutes for a persistently failing
// endpoint), none of it configurable through [tfe.Config]; a 429 is retried
// regardless of that flag, with every 429'd request sleeping to the same
// reset instant and stampeding the reopened window; and its reset-header
// parse exits the process on a malformed value. The client therefore
// disables go-tfe's server-error retry and owns the whole policy here, at
// the transport, where the status code still exists (the go-tfe error
// rendering discards it).
//
// Server-error retries back off locally with a bounded doubling delay.
// Rate-limit retries carry no local backoff: the [Governor] below has
// already recorded the server's reset, so re-entering the transport blocks
// there until the window reopens. A 429 that will not be retried -- its
// budget spent, or a request unsafe to re-send -- converts to an error
// wrapping [ErrRateLimited] rather than returning the response, so go-tfe
// never sees a 429 and its own rate-limit machinery stays dormant.
//
// Only a request with no body (every read this client makes) is retried, so a
// consumed body is never re-sent. A context cancellation ends the retries
// immediately.
type retryTransport struct {
	next             http.RoundTripper
	delay            time.Duration
	retries          int
	rateLimitRetries int
}

// RoundTrip delegates to the wrapped transport, retrying each retryable
// failed attempt within its budget. The final attempt's response or error is
// returned as-is -- except a final 429, which converts to an error wrapping
// [ErrRateLimited] so the layers above never see one.
func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	delay := t.delay

	var serverAttempts, rateLimitAttempts int

	for {
		resp, err := t.next.RoundTrip(req)

		if err == nil && resp.StatusCode == http.StatusTooManyRequests {
			drainBody(resp)

			if rateLimitAttempts < t.rateLimitRetries && retryableRequest(req) {
				rateLimitAttempts++

				continue
			}

			return nil, fmt.Errorf("%w (%d attempt(s))", ErrRateLimited, rateLimitAttempts+1)
		}

		if serverAttempts >= t.retries || !retryableAttempt(req, resp, err) {
			return resp, err //nolint:wrapcheck // A transparent transport wrapper.
		}

		serverAttempts++

		drainBody(resp)

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

// drainBody reads a little of a failed attempt's body and closes it, so the
// connection can be reused for the retry. A nil response is a no-op.
func drainBody(resp *http.Response) {
	if resp == nil {
		return
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit)) //nolint:errcheck // Best-effort drain.
	_ = resp.Body.Close()                                             //nolint:errcheck // Best-effort close.
}

// retryableRequest reports whether req is safe to re-send at all. Only a
// body-less request qualifies (a consumed body would re-send empty; every
// request this archiver makes is a body-less read), and a request whose
// context is already done is never retried, so a cancellation is not
// mistaken for a server fault.
func retryableRequest(req *http.Request) bool {
	if req.Body != nil && req.Body != http.NoBody {
		return false
	}

	return req.Context().Err() == nil
}

// retryableAttempt reports whether one attempt's outcome should be retried
// under the server-error budget: a transport-level failure or a server error
// (5xx), on a request that is safe to re-send.
func retryableAttempt(req *http.Request, resp *http.Response, err error) bool {
	if !retryableRequest(req) {
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
