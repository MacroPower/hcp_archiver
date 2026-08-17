package tfeclient

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// DefaultIdleReadTimeout bounds how long a response body read may sit with no
// bytes arriving before the read is abandoned, so a connection that stalls
// mid-body cannot wedge a worker (and with it the whole run) forever. It is an
// idle bound, not a total bound: a legitimately slow large transfer that keeps
// dribbling bytes is never capped, only silence is. It sits comfortably above
// [DefaultResponseHeaderTimeout], so a connection that has already produced
// headers gets at least as much patience per read as the initial header wait.
const DefaultIdleReadTimeout = 90 * time.Second

var (
	// ErrIdleReadTimeout is the sentinel wrapped by the error a response body
	// read returns when no bytes arrived within the idle-read timeout. The
	// returned error also implements [net.Error] with Timeout reporting true, so
	// [Classify] recognizes it as [KindTransient] and a re-run retries the
	// object.
	ErrIdleReadTimeout = errors.New("read stalled past idle timeout")

	// The idle error must satisfy [net.Error] or the timeout classification
	// never sees it.
	_ net.Error = (*idleReadError)(nil) //nolint:errcheck // Compile-time interface assertion.
)

// idleReadError is the error an [idleBody] read returns once its idle timer
// has fired. It wraps [ErrIdleReadTimeout] and implements [net.Error] with
// Timeout reporting true, so [Classify] reports it [KindTransient].
type idleReadError struct {
	timeout time.Duration
}

// Error renders the sentinel text with the configured bound.
func (e *idleReadError) Error() string {
	return fmt.Sprintf("%s (%s)", ErrIdleReadTimeout, e.timeout)
}

// Unwrap exposes [ErrIdleReadTimeout] to [errors.Is].
func (e *idleReadError) Unwrap() error { return ErrIdleReadTimeout }

// Timeout reports true so the error classifies as a transient network timeout.
func (e *idleReadError) Timeout() bool { return true }

// Temporary reports true; the stall is retryable on a fresh connection.
func (e *idleReadError) Temporary() bool { return true }

// throttleTransport paces every outbound attempt through the run's shared
// [Governor]s and feeds every response back to the governor that paced it.
//
// It lives at the transport rather than in [Client.Do] so that no request path
// outruns the throttle: the pages go-tfe's ListAll methods fetch internally
// all run inside one Do call, and the chunked log readers it hands back issue
// fresh requests mid-stream after Do has already returned. Each of those
// attempts passes here, pays its own token, and reports its outcome, so the
// governor sees exactly one signal per physical attempt. A cancellation while
// waiting surfaces the context's error, which classifies transient.
//
// The server does not meter all endpoints from one bucket: the two runs list
// endpoints have their own budget sixty times below the general one (see
// [DefaultRunsListRateLimit]), so they are paced by their own governor. The
// split also keeps each bucket's 429 feedback its own: a runs-list 429 must not
// pause or halve general traffic that has server headroom, and vice versa.
//
// It also strips X-RateLimit-Limit from every response. The go-tfe client
// reads that header once, during construction, to configure an internal rate
// limiter of its own (at a fraction of the advertised limit, waited on for
// every request); with the header gone that limiter configures to infinity
// and never blocks, leaving the governors the one authority over the rate.
type throttleTransport struct {
	next    http.RoundTripper
	gov     *Governor
	runsGov *Governor
}

// RoundTrip waits for a rate token from the governor owning the request's
// endpoint, delegates to the wrapped transport, and reports the response's
// outcome back to that governor.
func (t *throttleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	gov := t.governorFor(req)

	err := gov.Wait(req.Context())
	if err != nil {
		return nil, err
	}

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return resp, err //nolint:wrapcheck // A transparent transport wrapper.
	}

	resp.Header.Del(headerRateLimitLimit)

	if resp.StatusCode == http.StatusTooManyRequests {
		gov.On429(resp.Header.Get(headerRateLimitReset))
	} else {
		gov.OnSuccess()
	}

	return resp, nil
}

// governorFor returns the governor owning the request's endpoint: the runs-list
// governor for the two runs list endpoints, the general governor for everything
// else. A nil runs-list governor routes everything through the general one.
func (t *throttleTransport) governorFor(req *http.Request) *Governor {
	if t.runsGov != nil && runsListPath(req.URL.Path) {
		return t.runsGov
	}

	return t.gov
}

// runsListPath reports whether path is one of the two runs list endpoints, GET
// /workspaces/:workspace_id/runs or GET /organizations/:name/runs, which the
// server meters in their own 30-requests-per-minute bucket. The match is on the
// trailing path segments, so a Terraform Enterprise base path prefix does not
// defeat it, and neither run reads (/runs/:id) nor run creation (/runs) match.
//
// This decides pacing only. A caller of either endpoint must also take its slot
// from the matching gate by going through [Client.DoRunsList] rather than
// [Client.Do]: a slot is held across the wait this classification imposes, so a
// runs list request on the general gate parks a general slot for seconds.
func runsListPath(path string) bool {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	n := len(segs)

	if n < 3 || segs[n-1] != "runs" || segs[n-2] == "" {
		return false
	}

	return segs[n-3] == "workspaces" || segs[n-3] == "organizations"
}

// idleTransport wraps every response body in an [idleBody], bounding mid-body
// stalls and optionally counting raw wire bytes as they arrive.
//
// The idle bound works by closing the body from a timer, which unblocks a
// pending Read only under HTTP/1 connection semantics; the client this wraps
// keeps ForceAttemptHTTP2 disabled (see resolveHTTPClient), and re-enabling
// HTTP/2 would silently break the bound.
type idleTransport struct {
	next        http.RoundTripper
	logger      *slog.Logger
	wireBytes   *atomic.Int64
	idleTimeout time.Duration
}

// RoundTrip delegates to the wrapped transport and instruments the response
// body of any successful exchange; the [http.RoundTripper] contract guarantees
// a non-nil body when the error is nil. It sits below every retry loop, so a
// retried request is observed here once per attempt even though the caller
// only ever sees the final outcome.
func (t *idleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	resp, err := t.next.RoundTrip(req)

	t.logRoundTrip(req, resp, err, time.Since(start))

	if err != nil {
		return resp, err //nolint:wrapcheck // A transparent transport wrapper.
	}

	resp.Body = &idleBody{
		body:    resp.Body,
		timeout: t.idleTimeout,
		wire:    t.wireBytes,
	}

	return resp, nil
}

// logRoundTrip emits one debug line for the attempt that just completed on the
// wire, carrying the method, URL, elapsed time, and the status the attempt saw
// (or the transport error when it never got a response). It sits below the
// retry layers, so a retried request logs once per attempt, and the elapsed
// time covers only the headers: the body streams to the caller after
// RoundTrip returns.
func (t *idleTransport) logRoundTrip(req *http.Request, resp *http.Response, err error, elapsed time.Duration) {
	if t.logger == nil {
		return
	}

	attrs := []slog.Attr{
		slog.String("method", req.Method),
		slog.String("url", req.URL.Redacted()),
		slog.Duration("duration", elapsed),
	}

	switch {
	case err != nil:
		attrs = append(attrs, slog.String("error", err.Error()))
	case resp.StatusCode == http.StatusTooManyRequests:
		// The limit header names the size of the bucket that throttled the
		// request, so an endpoint metered outside the documented buckets
		// identifies itself in the trace instead of masquerading as the general
		// limit. This transport sits below the throttle wrapper that strips the
		// limit header, so the value is still present here.
		attrs = append(attrs,
			slog.Int("status", resp.StatusCode),
			slog.String("ratelimit_limit", resp.Header.Get(headerRateLimitLimit)),
			slog.String("ratelimit_reset", resp.Header.Get(headerRateLimitReset)),
		)

	default:
		attrs = append(attrs, slog.Int("status", resp.StatusCode))
	}

	t.logger.LogAttrs(req.Context(), slog.LevelDebug, "http_request", attrs...)
}

// idleBody is a response body whose every read is bounded by an idle timer:
// a read that sits longer than the timeout with no bytes arriving is unblocked
// by closing the underlying body and reports an [idleReadError].
type idleBody struct {
	body    io.ReadCloser
	wire    *atomic.Int64
	timer   *time.Timer
	timeout time.Duration
	fired   atomic.Bool
}

// Read arms the idle timer, delegates, and counts the bytes read into the wire
// counter when one is set. Once the timer has fired the body is closed, and
// every read error from then on is substituted with the idle-timeout error:
// the interrupted read itself, and also a later read hitting the already-closed
// body when the timer fired just as a read returned successfully. Without the
// sticky flag, that later "read on closed response body" error would classify
// as unknown and lose the transient label. A clean EOF is never substituted:
// the transfer completed, however close the race.
func (b *idleBody) Read(p []byte) (int, error) {
	// Reads are serial (io.Copy drives one at a time), so a single timer is
	// armed once and reset per read rather than allocated and dropped on the
	// timer heap every call, tens of thousands of times for a large blob.
	if b.timer == nil {
		b.timer = time.AfterFunc(b.timeout, func() {
			b.fired.Store(true)
			b.body.Close() //nolint:errcheck,gosec // Best-effort unblock of a stalled read.
		})
	} else {
		b.timer.Reset(b.timeout)
	}

	n, err := b.body.Read(p)

	if !b.timer.Stop() {
		b.fired.Store(true)
	}

	if n > 0 && b.wire != nil {
		b.wire.Add(int64(n))
	}

	if err != nil && !errors.Is(err, io.EOF) && b.fired.Load() {
		err = &idleReadError{timeout: b.timeout}
	}

	return n, err
}

// Close closes the underlying body; closing after the idle timer already
// closed it is harmless.
func (b *idleBody) Close() error {
	return b.body.Close() //nolint:wrapcheck // A transparent body wrapper.
}
