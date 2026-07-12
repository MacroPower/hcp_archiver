package workpool

import (
	"context"
	"time"
)

// DefaultInterval is the cadence at which a [Controller] samples the
// rate-limit signal and adjusts its pool.
const DefaultInterval = 10 * time.Second

// Controller scales a [Pool] from a rate-limit signal.
//
// On each tick it samples a cumulative counter of rate-limited responses and
// adjusts the pool the way TCP finds a path's capacity. Until the first rate
// limiting is ever observed the pool doubles on every clean window (slow
// start), so a run that begins at one worker reaches a useful parallelism in
// a few windows rather than minutes. From the first throttle on it moves
// additively-increase, multiplicatively-decrease: a window that saw any rate
// limiting halves the worker count (never below the floor), and a clean
// window adds one worker (never above the ceiling). Halving sheds load
// quickly when the server pushes back, while the one-at-a-time recovery
// probes back up gently enough not to oscillate.
//
// Create instances with [NewController].
type Controller struct {
	pool      *Pool
	sample    func() int64
	onResize  func(prev, next int, hits int64)
	interval  time.Duration
	min       int
	max       int
	lastSeen  int64
	throttled bool
}

// ControllerOption configures a [Controller] passed to [NewController].
//
// The available options are:
//   - [WithBounds]
//   - [WithInterval]
//   - [WithOnResize]
type ControllerOption func(*Controller)

// WithBounds sets the floor and ceiling the controller scales the pool
// between. A floor below one is raised to one, and a ceiling below the floor
// is raised to the floor. It returns a [ControllerOption].
func WithBounds(floor, ceiling int) ControllerOption {
	return func(c *Controller) {
		c.min = max(1, floor)
		c.max = max(c.min, ceiling)
	}
}

// WithInterval sets the sampling cadence, defaulting to [DefaultInterval]. A
// non-positive value keeps the default. It returns a [ControllerOption].
func WithInterval(d time.Duration) ControllerOption {
	return func(c *Controller) {
		if d > 0 {
			c.interval = d
		}
	}
}

// WithOnResize sets a callback invoked after each resize with the previous
// and new worker counts and the rate-limit hits observed in the window that
// triggered it, so the caller can log scaling decisions. A nil callback
// disables it. It returns a [ControllerOption].
func WithOnResize(fn func(prev, next int, hits int64)) ControllerOption {
	return func(c *Controller) {
		c.onResize = fn
	}
}

// NewController creates a new [Controller] scaling pool from sample, a
// cumulative count of rate-limited responses (such as the counter the API
// client fills through its rate-limit option).
//
// Without [WithBounds] the controller keeps the floor at one and the ceiling
// at the pool's size at construction, so by default it only sheds and recovers
// and never grows past the configured concurrency.
func NewController(pool *Pool, sample func() int64, opts ...ControllerOption) *Controller {
	c := &Controller{
		pool:     pool,
		sample:   sample,
		interval: DefaultInterval,
		min:      1,
		max:      pool.Size(),
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// Run samples and adjusts the pool on the configured cadence until ctx is
// done. The pool keeps its last size after Run returns.
func (c *Controller) Run(ctx context.Context) {
	// Anchor the window so hits accumulated before Run (a construction-to-start
	// gap) are not charged to the first tick.
	c.lastSeen = c.sample()

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			c.step()
		}
	}
}

// step samples the window and applies one scaling decision: halve on any
// rate-limit hits, and grow on a clean window, doubling while the run has
// never been throttled, then one worker at a time after.
func (c *Controller) step() {
	total := c.sample()
	hits := total - c.lastSeen
	c.lastSeen = total

	prev := c.pool.Size()

	var next int

	switch {
	case hits > 0:
		c.throttled = true
		next = min(c.max, max(c.min, prev/2))

	case c.throttled:
		next = min(c.max, prev+1)
	default:
		next = min(c.max, prev*2)
	}

	if next == prev {
		return
	}

	c.pool.Resize(next)

	if c.onResize != nil {
		c.onResize(prev, next, hits)
	}
}
