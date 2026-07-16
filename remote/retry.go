package remote

import (
	"context"
	"time"

	"gocloud.dev/gcerrors"
)

// Default in-client retry settings for transient store failures.
const (
	// DefaultRetries is the default number of additional attempts each store
	// operation makes after a failure that classifies transient.
	DefaultRetries = 3

	// DefaultRetryDelay is the default wait before an operation's first
	// retry; each subsequent retry doubles it, capped at maxRetryDelay.
	DefaultRetryDelay = 1 * time.Second

	// The maxRetryDelay cap bounds the doubling retry backoff so a generously
	// configured retry count cannot stretch one operation's stall into
	// minutes.
	maxRetryDelay = 5 * time.Second
)

// withRetry runs op, retrying each transient failure under the client's
// bounded doubling backoff until the budget is spent. It exists because the
// mirror is the archive's long-term record and its writes deserve the same
// in-run persistence the API transport gives fetches: without it, one blip
// during the close sweep defers a file to a run that may be days away. The
// provider SDKs retry beneath it for s3:// and azblob://, so this layer only
// catches what those give up on; file:// has no retry of its own at all.
//
// The final attempt's error is returned as-is; op owns any wrapping. A
// context cancellation ends the retries immediately, and each op must rewind
// whatever state an attempt consumes (a seekable body) before it retries.
func (c *Client) withRetry(ctx context.Context, op func() error) error {
	delay := c.retryDelay

	for attempt := 0; ; attempt++ {
		err := op()
		if err == nil || attempt >= c.retries || !retryable(ctx, err) {
			return err
		}

		werr := waitRetry(ctx, delay)
		if werr != nil {
			return err
		}

		delay = min(delay*2, maxRetryDelay)
	}
}

// retryable reports whether one attempt's failure is worth another try: a
// fault of the store or the path to it (an internal error, a throttle, an
// attempt that timed out, anything left unclassified, the shape a transport
// failure takes) on a call whose own context is still live. An error the
// store pins on the request itself — an absent key, a permission denial, a
// failed precondition such as a digest mismatch at commit — would only fail
// identically again and never retries, so an absence is settled by one
// response and a misconfiguration surfaces immediately.
func retryable(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}

	switch gcerrors.Code(err) {
	case gcerrors.NotFound,
		gcerrors.AlreadyExists,
		gcerrors.InvalidArgument,
		gcerrors.PermissionDenied,
		gcerrors.FailedPrecondition,
		gcerrors.Unimplemented,
		gcerrors.Canceled:
		return false
	default:
		return true
	}
}

// waitRetry sleeps d or until ctx is done, whichever comes first, returning
// the context's error when it ended the wait. A non-positive d only checks
// the context.
func waitRetry(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err() //nolint:wrapcheck // A transparent retry helper.
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // A transparent retry helper.
	case <-timer.C:
		return nil
	}
}
