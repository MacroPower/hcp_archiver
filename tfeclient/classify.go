package tfeclient

import (
	"context"
	"errors"
	"net"

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
)

// String returns the lowercase name of the [Kind].
func (k Kind) String() string {
	switch k {
	case KindTransient:
		return "transient"
	case KindTerminal:
		return "terminal"
	case KindUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Classify reports the [Kind] of err.
//
// It recognizes [tfe.ErrResourceNotFound] as [KindTerminal] and
// context cancellation, deadlines, and network timeouts as [KindTransient].
// A nil error and any error not matched structurally are [KindUnknown].
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

// isTimeout reports whether err unwraps to a timeout [net.Error].
func isTimeout(err error) bool {
	var netErr net.Error

	return errors.As(err, &netErr) && netErr.Timeout()
}
