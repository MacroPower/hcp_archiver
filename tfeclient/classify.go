package tfeclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/hashicorp/go-tfe"
)

var (
	// ErrMissingToken is returned by [New] when no API token was supplied.
	ErrMissingToken = errors.New("token is required")

	// ErrMissingLogURL is returned by [Client.OpenPlanLog] and
	// [Client.OpenApplyLog] when the read plan or apply carries no signed log
	// URL to fetch.
	ErrMissingLogURL = errors.New("log URL is missing")

	// The errForbidden403 sentinel is joined with a raw [tfe.ClientRequest.DoRaw]
	// HTTP error so the combined error's first line reads "403 Forbidden", the
	// text [isForbidden] recognizes. A plain wrap would let the root walk unwrap
	// past it to the generic "error HTTP response: 403"; [errors.Join] is used
	// instead because its single-error [errors.Unwrap] stops at the join, whose
	// Error begins with this line.
	errForbidden403 = errors.New("403 Forbidden")
)

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

	// KindTerminal is a permanent absence, such as a 404 surfaced as
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
// an HTTP 403. The status code is not preserved on the error, so the client
// renders a 403 as a bare error whose text is the joined JSON:API payload titles
// ("forbidden", optionally with a detail on the following lines) or the raw
// status line ("403 Forbidden"), which is the only structural signal available.
//
// The match runs against the unwrapped root cause, one line at a time, so an
// archive path or an unrelated error body that merely contains the word (a path
// segment like "forbidden-experiments", a 500 whose detail mentions it) is not
// misclassified as an access denial.
func isForbidden(err error) bool {
	if err == nil {
		return false
	}

	root := err

	for {
		unwrapped := errors.Unwrap(root)
		if unwrapped == nil {
			break
		}

		root = unwrapped
	}

	for line := range strings.SplitSeq(root.Error(), "\n") {
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "forbidden", "403 forbidden":
			return true
		}
	}

	return false
}

// statusError translates a raw [tfe.ClientRequest.DoRaw] HTTP error, whose text
// is only a generic "error HTTP response: N", into the typed error the SDK's
// checked request path yields for that status, so [Classify] yields the same
// [Kind]. Only the statuses that path special-cases are mapped:
//
//   - 404 wraps [tfe.ErrResourceNotFound] ([KindTerminal], recorded absent). 410
//     is deliberately left unmapped, since the checked path does not special-case
//     it either.
//   - 403 joins [errForbidden403] ([KindForbidden]).
//   - 401 wraps [tfe.ErrUnauthorized] (classification-neutral; both yield
//     [KindUnknown], the wrap only preserves the typed error).
//
// Every other status returns orig unchanged ([KindUnknown]), as the checked path
// does for unusual statuses.
func statusError(status int, orig error) error {
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %w", tfe.ErrResourceNotFound, orig)
	case http.StatusForbidden:
		return errors.Join(errForbidden403, orig)
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %w", tfe.ErrUnauthorized, orig)
	default:
		return orig
	}
}
