package tfeclient

import "net/http"

// ResolveHTTPClient builds the HTTP client [New] would construct from opts,
// exposing the response-header-timeout wiring to the external test package
// directly rather than through the retrying go-tfe client (whose retries would
// amplify a header-timeout failure).
func ResolveHTTPClient(opts ...Option) *http.Client {
	cfg := newConfig(opts)

	return resolveHTTPClient(&cfg)
}
