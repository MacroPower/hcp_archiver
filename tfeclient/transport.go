package tfeclient

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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

// idleTransport wraps every response body in an [idleBody], bounding mid-body
// stalls and optionally counting raw wire bytes as they arrive.
//
// The idle bound works by closing the body from a timer, which unblocks a
// pending Read only under HTTP/1 connection semantics; the client this wraps
// keeps ForceAttemptHTTP2 disabled (see resolveHTTPClient), and re-enabling
// HTTP/2 would silently break the bound.
type idleTransport struct {
	next        http.RoundTripper
	wireBytes   *atomic.Int64
	idleTimeout time.Duration
}

// RoundTrip delegates to the wrapped transport and instruments the response
// body of any successful exchange; the [http.RoundTripper] contract guarantees
// a non-nil body when the error is nil.
func (t *idleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
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

// idleBody is a response body whose every read is bounded by an idle timer:
// a read that sits longer than the timeout with no bytes arriving is unblocked
// by closing the underlying body and reports an [idleReadError].
type idleBody struct {
	body    io.ReadCloser
	wire    *atomic.Int64
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
	timer := time.AfterFunc(b.timeout, func() {
		b.fired.Store(true)
		b.body.Close() //nolint:errcheck,gosec // Best-effort unblock of a stalled read.
	})

	n, err := b.body.Read(p)

	if !timer.Stop() {
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
