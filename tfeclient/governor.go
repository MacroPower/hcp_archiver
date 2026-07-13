package tfeclient

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Rate-limit header names, in Go's canonical MIME form (the wire spells them
// X-RateLimit-*; [http.Header] lookups canonicalize either way).
const (
	headerRateLimitLimit = "X-Ratelimit-Limit"
	headerRateLimitReset = "X-Ratelimit-Reset"
)

// Tuning constants for the adaptive rate [Governor].
const (
	// GovernorRaiseInterval is how long a [Governor] must go without a rate
	// decrease or a raise before it adds one request per second, so recovery
	// probes upward gently (floor to ceiling in about a minute) instead of
	// re-triggering the throttle it just backed away from.
	GovernorRaiseInterval = 2 * time.Second

	// GovernorMaxPause caps the cooldown a single X-RateLimit-Reset header may
	// impose on a [Governor], so a nonsense value cannot park the whole run
	// for minutes.
	GovernorMaxPause = 60 * time.Second

	// GovernorFallbackPause is the cooldown a [Governor] applies when a
	// rate-limited response carries no parseable reset header: long enough to
	// clear a one-second limit window, short enough to not stall a mistaken
	// parse.
	GovernorFallbackPause = 2 * time.Second
)

// Governor is the run's single adaptive rate authority: a token bucket whose
// rate moves with the server's feedback.
//
// Every physical HTTP attempt pays one token through [Governor.Wait] before
// launching. A rate-limited (429) response reported through [Governor.On429]
// halves the rate (at most once per cooldown window, never below the floor),
// drains the bucket, and pauses all launches until the server's advertised
// reset, because the server counts rejected requests against the window too:
// pacing into a blown window only farms more 429s, so the only useful
// response is a global pause. Any other response, reported through
// [Governor.OnSuccess], is evidence of restored capacity, and after a clean
// stretch the rate creeps back up one request per second toward the ceiling.
//
// It is a custom bucket rather than a [golang.org/x/time/rate.Limiter]
// because the feedback path needs operations that limiter cannot express:
// draining the bucket on a 429, and re-pacing waiters already parked in Wait
// when the rate drops. Create instances with [NewGovernor]. A Governor is
// safe for concurrent use.
type Governor struct {
	now          func() time.Time
	rateLimited  *atomic.Int64
	last         time.Time
	pauseUntil   time.Time
	lastRaise    time.Time
	lastDecrease time.Time
	mu           sync.Mutex
	rate         float64
	tokens       float64
	ceiling      float64
	floor        float64
}

// GovernorOption configures a [Governor] passed to [NewGovernor].
//
// Options of this type:
//   - [WithGovernorClock]
type GovernorOption func(*Governor)

// WithGovernorClock sets the time source the governor's refill, cooldown, and
// raise logic read, defaulting to [time.Now]. Inject a deterministic clock in
// tests. It returns a [GovernorOption].
func WithGovernorClock(now func() time.Time) GovernorOption {
	return func(g *Governor) {
		if now != nil {
			g.now = now
		}
	}
}

// NewGovernor creates a new [Governor] whose rate starts at ceiling, the
// fastest it will ever launch, and adapts downward from the server's
// feedback. The floor is one request per second, or the ceiling itself when
// that is lower, so a deliberately tiny ceiling still binds. Each 429
// reported to the governor increments rateLimited; a nil counter disables the
// counting.
func NewGovernor(ceiling float64, rateLimited *atomic.Int64, opts ...GovernorOption) *Governor {
	g := &Governor{
		now:         time.Now,
		rateLimited: rateLimited,
		rate:        ceiling,
		ceiling:     ceiling,
		floor:       math.Min(1, ceiling),
	}

	for _, opt := range opts {
		opt(g)
	}

	g.tokens = g.burstLocked()

	return g
}

// burstLocked is the bucket's capacity at the current rate: a fraction of one
// second's sustained tokens, because the server enforces its limit per second
// and a full second's saved-up burst released on top of the sustained stream
// would land inside one window and read back as 429s. The caller must hold mu
// (or be the constructor, before the governor is shared).
func (g *Governor) burstLocked() float64 {
	return math.Max(1, g.rate/3)
}

// refillLocked accrues tokens for the time elapsed since the last refill,
// capped at the burst. The caller must hold mu.
func (g *Governor) refillLocked(now time.Time) {
	if !g.last.IsZero() {
		if elapsed := now.Sub(g.last).Seconds(); elapsed > 0 {
			g.tokens = math.Min(g.tokens+elapsed*g.rate, g.burstLocked())
		}
	}

	g.last = now
}

// Wait blocks until the caller may launch one request: past any cooldown and
// holding one token. It re-checks after every sleep because a concurrent 429
// can extend the cooldown or drop the rate while a waiter is parked. A
// cancellation while waiting returns ctx's error wrapped.
func (g *Governor) Wait(ctx context.Context) error {
	for {
		g.mu.Lock()

		now := g.now()
		g.refillLocked(now)

		var delay time.Duration

		switch {
		case now.Before(g.pauseUntil):
			delay = g.pauseUntil.Sub(now)
		case g.tokens >= 1:
			g.tokens--
			g.mu.Unlock()

			return nil

		default:
			delay = time.Duration((1 - g.tokens) / g.rate * float64(time.Second))
		}

		g.mu.Unlock()

		// A sub-millisecond wait sleeps a millisecond instead, so float
		// truncation cannot spin the loop hot while the last fraction of a
		// token accrues.
		err := waitRetry(ctx, max(delay, time.Millisecond))
		if err != nil {
			return fmt.Errorf("rate governor wait: %w", err)
		}
	}
}

// On429 records one rate-limited response given its X-RateLimit-Reset header
// value: it extends the global cooldown to the server's advertised reset,
// halves the rate once per cooldown window, and drains the bucket so
// post-cooldown re-entry serializes at the new rate.
//
// The halving is guarded on the window, not the response: a concurrency-wide
// burst of 429s from one blown window is one signal, and halving per response
// would collapse the rate to the floor instantly.
func (g *Governor) On429(resetHeader string) {
	pause := parseRateLimitReset(resetHeader)

	g.mu.Lock()

	now := g.now()
	g.refillLocked(now)

	if !now.Before(g.pauseUntil) {
		g.rate = math.Max(g.floor, g.rate/2)
		g.lastDecrease = now
	}

	if until := now.Add(pause); until.After(g.pauseUntil) {
		g.pauseUntil = until
	}

	g.tokens = 0
	g.mu.Unlock()

	if g.rateLimited != nil {
		g.rateLimited.Add(1)
	}
}

// OnSuccess records one response that was not rate limited. Any such response
// consumed server window capacity without pushback -- a 403 or a 500 is as
// much evidence of rate headroom as a 200 -- so after a clean stretch
// (outside any cooldown, and [GovernorRaiseInterval] past both the last raise
// and the last decrease) the rate creeps up one request per second toward the
// ceiling. Recovery is event-driven by design: an idle run needs no rate.
func (g *Governor) OnSuccess() {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()

	if now.Before(g.pauseUntil) ||
		now.Sub(g.lastRaise) < GovernorRaiseInterval ||
		now.Sub(g.lastDecrease) < GovernorRaiseInterval {
		return
	}

	// Settle the bucket at the old rate before moving it, so the elapsed
	// stretch is not re-priced at the new one.
	g.refillLocked(now)

	g.rate = math.Min(g.ceiling, g.rate+1)
	g.lastRaise = now
}

// Snapshot returns the current adaptive rate in requests per second and how
// much of a cooldown pause remains (zero when launches are flowing), for the
// progress views.
func (g *Governor) Snapshot() (float64, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var paused time.Duration

	if now := g.now(); now.Before(g.pauseUntil) {
		paused = g.pauseUntil.Sub(now)
	}

	return g.rate, paused
}

// parseRateLimitReset reads an X-RateLimit-Reset header value (fractional
// seconds until the server's limit window reopens) into a cooldown duration,
// clamped to [0, GovernorMaxPause]. A missing or unparseable header falls
// back to [GovernorFallbackPause]; it is never fatal, unlike go-tfe's own
// parse of the same header.
func parseRateLimitReset(value string) time.Duration {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return GovernorFallbackPause
	}

	return min(max(time.Duration(seconds*float64(time.Second)), 0), GovernorMaxPause)
}
