package tfeclient_test

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// testClock is a deterministic time source the governor tests advance by hand.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock {
	return &testClock{at: time.Unix(1_000_000, 0)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.at
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.at = c.at.Add(d)
}

func TestGovernorHalvesOncePerCooldownWindow(t *testing.T) {
	t.Parallel()

	// A concurrency-wide burst of 429s from one blown window is one signal:
	// sixteen simultaneous reports must halve the rate exactly once, not
	// collapse it to the floor.
	clock := newTestClock()
	g := tfeclient.NewGovernor(30, nil, tfeclient.WithGovernorClock(clock.Now))

	for range 16 {
		g.On429("5")
	}

	rps, paused := g.Snapshot()

	assert.InEpsilon(t, 15.0, rps, 1e-9, "one halving per window")
	assert.Equal(t, 5*time.Second, paused)
	assert.Zero(t, tfeclient.GovernorTokens(g), "the bucket drains on a 429")

	// A fresh window after the cooldown halves again.
	clock.Advance(6 * time.Second)
	g.On429("5")

	rps, _ = g.Snapshot()
	assert.InEpsilon(t, 7.5, rps, 1e-9)
}

func TestGovernorHalvesOncePerZeroResetBurst(t *testing.T) {
	t.Parallel()

	// A burst of 429s advertising an immediate (zero) reset is still one blown
	// window: it must halve once, not once per response down to the floor, even
	// though a zero reset cannot push the cooldown into the future.
	clock := newTestClock()
	g := tfeclient.NewGovernor(30, nil, tfeclient.WithGovernorClock(clock.Now))

	for range 16 {
		g.On429("0")
	}

	rps, paused := g.Snapshot()

	assert.InEpsilon(t, 15.0, rps, 1e-9, "one halving for the whole burst")
	assert.Zero(t, paused, "a zero reset advertises no cooldown")
}

func TestGovernorCooldownExtendsMonotonically(t *testing.T) {
	t.Parallel()

	// Concurrent 429s carry different resets; the cooldown keeps the furthest
	// one and never moves backward.
	clock := newTestClock()
	g := tfeclient.NewGovernor(30, nil, tfeclient.WithGovernorClock(clock.Now))

	g.On429("2")
	g.On429("5")
	g.On429("1")

	_, paused := g.Snapshot()
	assert.Equal(t, 5*time.Second, paused, "the furthest reset wins; a shorter one never shrinks it")
}

func TestGovernorResetHeaderParsing(t *testing.T) {
	t.Parallel()

	// The pause each header value yields, observed through the snapshot. The
	// parse must never be fatal, unlike go-tfe's own handling of the header.
	tests := map[string]struct {
		reset string
		want  time.Duration
	}{
		"fractional seconds":      {reset: "1.5", want: 1500 * time.Millisecond},
		"zero is no pause":        {reset: "0", want: 0},
		"negative clamps to zero": {reset: "-3", want: 0},
		"huge clamps to the cap":  {reset: "3600", want: tfeclient.GovernorMaxPause},
		// A finite value big enough to overflow the int64 nanosecond
		// conversion (e.g. an epoch-milliseconds timestamp) must clamp to the
		// cap on every architecture, not overflow to an arch-dependent value.
		"overflowing clamps to the cap": {reset: "1770000000000", want: tfeclient.GovernorMaxPause},
		"missing falls back":            {reset: "", want: tfeclient.GovernorFallbackPause},
		"unparseable falls back":        {reset: "soon", want: tfeclient.GovernorFallbackPause},
		"trailing junk falls back":      {reset: "1.5s", want: tfeclient.GovernorFallbackPause},
		"NaN falls back":                {reset: "NaN", want: tfeclient.GovernorFallbackPause},
		"positive infinity back":        {reset: "+Inf", want: tfeclient.GovernorFallbackPause},
		"negative infinity back":        {reset: "-Inf", want: tfeclient.GovernorFallbackPause},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			clock := newTestClock()
			g := tfeclient.NewGovernor(30, nil, tfeclient.WithGovernorClock(clock.Now))

			g.On429(tc.reset)

			_, paused := g.Snapshot()
			assert.Equal(t, tc.want, paused)
		})
	}
}

func TestGovernorRaisesOnCleanStretches(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	g := tfeclient.NewGovernor(30, nil, tfeclient.WithGovernorClock(clock.Now))

	// Drop the rate so there is headroom to recover into.
	g.On429("1")

	rps, _ := g.Snapshot()
	require.InEpsilon(t, 15.0, rps, 1e-9)

	// Still inside the cooldown: no raise.
	clock.Advance(500 * time.Millisecond)
	g.OnSuccess()

	rps, _ = g.Snapshot()
	assert.InEpsilon(t, 15.0, rps, 1e-9, "no raise during a cooldown")

	// Past the cooldown but within the raise interval of the decrease.
	clock.Advance(1 * time.Second)
	g.OnSuccess()

	rps, _ = g.Snapshot()
	assert.InEpsilon(t, 15.0, rps, 1e-9, "no raise within the interval of a decrease")

	// A clean stretch raises by one, and only once per interval.
	clock.Advance(tfeclient.GovernorRaiseInterval)
	g.OnSuccess()
	g.OnSuccess()

	rps, _ = g.Snapshot()
	assert.InEpsilon(t, 16.0, rps, 1e-9, "one raise per clean interval")

	clock.Advance(tfeclient.GovernorRaiseInterval)
	g.OnSuccess()

	rps, _ = g.Snapshot()
	assert.InEpsilon(t, 17.0, rps, 1e-9)
}

func TestGovernorRateNeverExceedsCeiling(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	g := tfeclient.NewGovernor(2, nil, tfeclient.WithGovernorClock(clock.Now))

	for range 5 {
		clock.Advance(tfeclient.GovernorRaiseInterval)
		g.OnSuccess()
	}

	rps, _ := g.Snapshot()
	assert.InEpsilon(t, 2.0, rps, 1e-9, "clean stretches never push the rate past the ceiling")
}

func TestGovernorFloorFollowsATinyCeiling(t *testing.T) {
	t.Parallel()

	// A ceiling below the usual one-request-per-second floor must still bind:
	// the floor drops to the ceiling rather than raising the rate above it.
	clock := newTestClock()
	g := tfeclient.NewGovernor(0.25, nil, tfeclient.WithGovernorClock(clock.Now))

	for range 3 {
		clock.Advance(10 * time.Second)
		g.On429("1")
	}

	rps, _ := g.Snapshot()
	assert.InEpsilon(t, 0.25, rps, 1e-9, "halving stops at the sub-1 floor set by the ceiling")
}

func TestGovernorNonPositiveCeilingStillAdmits(t *testing.T) {
	t.Parallel()

	// A ceiling that is no rate at all (zero, negative, NaN) reads as one
	// request per second. The governor's pacing divides by the rate, so a
	// rate of zero taken literally would compute an infinite delay and wedge
	// every request behind the first.
	for name, ceiling := range map[string]float64{
		"zero":     0,
		"negative": -3,
		"nan":      math.NaN(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			clock := newTestClock()
			g := tfeclient.NewGovernor(ceiling, nil, tfeclient.WithGovernorClock(clock.Now))

			rps, _ := g.Snapshot()
			assert.InEpsilon(t, 1.0, rps, 1e-9, "the rate settles at the one-per-second floor")

			// The second admission needs accrued tokens, not just the initial
			// burst, so it proves the delay arithmetic stays finite.
			require.NoError(t, g.Wait(t.Context()))
			clock.Advance(2 * time.Second)
			require.NoError(t, g.Wait(t.Context()))
		})
	}
}

func TestGovernorWaitSpendsBurstThenBlocks(t *testing.T) {
	t.Parallel()

	// The initial bucket holds a third of the ceiling; each admission spends
	// one token without touching the wall clock.
	clock := newTestClock()
	g := tfeclient.NewGovernor(30, nil, tfeclient.WithGovernorClock(clock.Now))

	for range 10 {
		require.NoError(t, g.Wait(t.Context()))
	}

	assert.Less(t, tfeclient.GovernorTokens(g), 1.0, "the burst is spent")
}

func TestGovernorWaitHonorsCancellation(t *testing.T) {
	t.Parallel()

	// The bucket is drained and a long cooldown is in force, so the wait can
	// only end through the context.
	g := tfeclient.NewGovernor(30, nil)
	g.On429("60")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := g.Wait(ctx)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second, "the cancellation ends the wait promptly")
}
