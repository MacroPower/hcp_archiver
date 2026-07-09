package tfeclient

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/hashicorp/go-tfe"
)

// ErrMissingToken is returned by [New] when no API token was supplied.
var ErrMissingToken = errors.New("token is required")

// Kind classifies an error so a resume can distinguish a temporary blip from a
// permanent absence.
//
// The zero value is [KindUnknown]: an error that cannot be recognized
// structurally is never reported as terminal, so callers that give up only on
// [KindTerminal] never mistake an unclassified failure for a gone object.
type Kind int

const (
	// KindUnknown is an error that could not be classified. It is the default
	// for anything not recognized, and callers should treat it as retryable.
	KindUnknown Kind = iota

	// KindTransient is a retryable error: context cancellation or deadline,
	// a network timeout, or rate-limiter exhaustion.
	KindTransient

	// KindTerminal is a permanent absence, such as a 404 or 410 surfaced as
	// [tfe.ErrResourceNotFound]. Retrying will not help.
	KindTerminal

	// KindForbidden is an access denial (an HTTP 403), such as an endpoint that
	// rejects a team or organization token. Retrying with the same identity will
	// not help, but retrying under a differently scoped token can, so callers
	// record it apart from a genuine error and keep it retryable.
	KindForbidden
)

// String returns the lowercase name of the [Kind].
func (k Kind) String() string {
	switch k {
	case KindTransient:
		return "transient"
	case KindTerminal:
		return "terminal"
	case KindForbidden:
		return "forbidden"
	case KindUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Classify reports the [Kind] of err.
//
// It recognizes [tfe.ErrResourceNotFound] as [KindTerminal]; context
// cancellation, deadlines, and network timeouts as [KindTransient]; and an
// access denial (an HTTP 403) as [KindForbidden]. A nil error and any error not
// matched structurally are [KindUnknown].
//
// The go-tfe v1 client discards the HTTP status of a 403 and surfaces only the
// joined error payload, whose title is "forbidden", so a forbidden error is
// recognized by that text rather than a sentinel or status code.
func Classify(err error) Kind {
	switch {
	case err == nil:
		return KindUnknown
	case errors.Is(err, tfe.ErrResourceNotFound):
		return KindTerminal
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return KindTransient
	case isTimeout(err):
		return KindTransient
	case isForbidden(err):
		return KindForbidden
	default:
		return KindUnknown
	}
}

// IsTerminal reports whether err is a permanent absence, i.e. its [Kind] is
// [KindTerminal].
func IsTerminal(err error) bool {
	return Classify(err) == KindTerminal
}

// IsTransient reports whether err is a retryable blip, i.e. its [Kind] is
// [KindTransient].
func IsTransient(err error) bool {
	return Classify(err) == KindTransient
}

// IsForbidden reports whether err is an access denial, i.e. its [Kind] is
// [KindForbidden].
func IsForbidden(err error) bool {
	return Classify(err) == KindForbidden
}

// isTimeout reports whether err unwraps to a timeout [net.Error].
func isTimeout(err error) bool {
	var netErr net.Error

	return errors.As(err, &netErr) && netErr.Timeout()
}

// isForbidden reports whether err carries the go-tfe v1 client's rendering of
// an HTTP 403, whose joined payload title is "forbidden". The status code is not
// preserved on the error, so the text is the only structural signal available.
func isForbidden(err error) bool {
	if err == nil {
		return false
	}

	return strings.Contains(strings.ToLower(err.Error()), "forbidden")
}
