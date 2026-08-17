package tfeclient

import (
	"io"
	"net/http"
)

// NewLogReader exposes newLogReader to the external test package, wrapping rc in
// the on-the-fly STX/ETX marker-trimming reader.
func NewLogReader(rc io.ReadCloser) io.ReadCloser {
	return newLogReader(rc)
}

// ResolveHTTPClient builds the HTTP client [New] would construct from opts,
// exposing the response-header-timeout wiring to the external test package
// directly rather than through the go-tfe client (whose constructor ping would
// amplify a header-timeout failure).
func ResolveHTTPClient(opts ...Option) *http.Client {
	cfg := newConfig(opts)

	return resolveHTTPClient(&cfg)
}

// UnderlyingTransport returns the [*http.Transport] beneath the retry,
// governor, and idle-bounding wrappers of a client built by
// [ResolveHTTPClient], so the external test package can assert the transport
// tuning (handshake bound, idle pool sizing) directly. It returns nil when the
// client does not carry the expected wrappers.
func UnderlyingTransport(hc *http.Client) *http.Transport {
	rr, ok := hc.Transport.(*retryTransport)
	if !ok {
		return nil
	}

	tt, ok := rr.next.(*throttleTransport)
	if !ok {
		return nil
	}

	it, ok := tt.next.(*idleTransport)
	if !ok {
		return nil
	}

	tr, ok := it.next.(*http.Transport)
	if !ok {
		return nil
	}

	return tr
}

// GovernorTokens reads g's current token balance without refilling, so a test
// can assert the bucket drained on a 429.
func GovernorTokens(g *Governor) float64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.tokens
}

// RunsListPath exposes runsListPath to the external test package.
func RunsListPath(path string) bool {
	return runsListPath(path)
}

// ThrottleGovernors returns the general and runs-list governors wired into a
// client built by [ResolveHTTPClient], in that order, so the external test
// package can assert which bucket a response's rate feedback landed in. Both
// are nil when the client does not carry the expected wrappers.
func ThrottleGovernors(hc *http.Client) (*Governor, *Governor) {
	rr, ok := hc.Transport.(*retryTransport)
	if !ok {
		return nil, nil
	}

	tt, ok := rr.next.(*throttleTransport)
	if !ok {
		return nil, nil
	}

	return tt.gov, tt.runsGov
}
