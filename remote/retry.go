package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
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

	// DefaultStallTimeout is the base window an attempt may go without
	// observable progress — a listed object delivered, a downloaded chunk
	// read — before the watchdog cancels it as stalled. A stalled attempt
	// classifies transient and retries, so a wedged connection costs one
	// window instead of hanging a worker, a seal, or the close sweep
	// forever; the mirror gets the same defense the API transport's
	// response-header timeout and idle-read watchdog give fetches.
	//
	// Writes cannot use the base window alone: their wire progress is
	// invisible from this side of the provider SDK (a sub-threshold body is
	// ingested into buffers at memory speed and transfers entirely inside
	// the writer's commit; a parted upload's tail drains the same way), so a
	// tight progress window would cut healthy-but-slow uploads that are
	// moving bytes the whole time — a false failure that would repeat on
	// every retry and every run, permanently blocking the mirror's
	// convergence. A write's window is therefore widened by the body's size
	// at [minWriteRate] (see [Client.writeWindow]): still a hard bound on a
	// wedged connection, never a cap on a working one.
	DefaultStallTimeout = 2 * time.Minute

	// DefaultServerCopyCap is the source size at and above which
	// [Client.Copy] streams the bytes instead of asking the backend for a
	// server-side copy: S3's single-request CopyObject rejects sources of
	// 5 GiB and up, and would do so identically on every retry and every
	// run, permanently blocking a heal's convergence for a large bundle.
	DefaultServerCopyCap = 5 << 30
)

// minWriteRate is the sustained transfer rate the write watchdog assumes no
// healthy link falls below: a write attempt's stall window is its base
// window plus bodySize/minWriteRate, so a link at or above this rate can
// never be cut mid-transfer no matter the body, while a truly wedged write
// is still bounded by the scaled deadline instead of hanging forever. The
// floor is deliberately conservative (32 KiB/s ≈ 0.26 Mbps); a link slower
// than that cannot sustain a mirror at all.
const minWriteRate = 32 << 10 // bytes per second

// writeWindow returns the stall window for one write attempt of size bytes:
// the base window widened to cover transferring the whole body at
// [minWriteRate], since everything past the SDK's buffers is unobservable
// progress. Zero when the watchdog is disabled.
func (c *Client) writeWindow(size int64) time.Duration {
	if c.stallTimeout <= 0 {
		return 0
	}

	return c.stallTimeout + time.Duration(size/minWriteRate)*time.Second
}

// errStalled marks an attempt the stall watchdog canceled: no progress
// landed within the client's stall window. It classifies transient — the
// path wedged, saying nothing about the object at the key — so the attempt
// retries.
var errStalled = errors.New("remote operation stalled")

// runAttempt runs one attempt of a store operation under the stall watchdog:
// op receives a context the watchdog cancels when the attempt goes window
// without calling touch, and an error from a stalled attempt is wrapped with
// [errStalled] so it classifies transient rather than as the cancellation
// the driver reports. Reads and listings pass the client's base window
// (their progress is observable per chunk or per object); writes pass
// [Client.writeWindow], sized so unobservable wire time can never trip it
// on a working link. A non-positive window disables the watchdog.
func (c *Client) runAttempt(
	ctx context.Context,
	window time.Duration,
	op func(ctx context.Context, touch func()) error,
) error {
	if window <= 0 {
		return op(ctx, func() {})
	}

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var stalled atomic.Bool

	timer := time.AfterFunc(window, func() {
		stalled.Store(true)
		cancel()
	})
	defer timer.Stop()

	// Resetting an AfterFunc timer is safe from concurrent readers; a touch
	// racing the firing at worst re-arms a timer whose context is already
	// canceled, which changes nothing.
	err := op(wctx, func() { timer.Reset(window) })
	if err != nil && stalled.Load() {
		return fmt.Errorf("%w (no progress for %s): %w", errStalled, window, err)
	}

	return err
}

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
// attempt that timed out, a stalled attempt the watchdog cut, anything left
// unclassified, the shape a transport failure takes) on a call whose own
// context is still live. An error the store pins on the request itself — an
// absent key, a permission denial, a failed precondition such as a digest
// mismatch at commit — would only fail identically again and never retries,
// so an absence is settled by one response and a misconfiguration surfaces
// immediately.
//
// A transport-level failure is classified before the store's code is
// consulted: a driver can mistake one for a request fault (azblob maps any
// "no such host" DNS failure to NotFound, guessing at an invalid account
// name), and trusting that guess would let a resolver blip settle sticky,
// destructive decisions — a delete counted as already-removed, a Head
// answering a permanent absence.
func retryable(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}

	if isTransportError(err) || errors.Is(err, errStalled) {
		return true
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

// isTransportError reports whether err is a network-path failure (a DNS
// resolution error, a dial or connection fault) rather than a response the
// store produced. Such an error says nothing about the object at the key, so
// it must never satisfy a NotFound check nor settle a request-fault
// classification, whatever gcerrors code a driver stamped on it.
func isTransportError(err error) bool {
	var (
		dnsErr *net.DNSError
		opErr  *net.OpError
	)

	return errors.As(err, &dnsErr) || errors.As(err, &opErr)
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
