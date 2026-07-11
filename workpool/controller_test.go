package workpool_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.jacobcolvin.com/hcp_archiver/workpool"
)

// stubSignal is a settable cumulative rate-limit counter driving a controller
// deterministically.
type stubSignal struct {
	total int64
}

func (s *stubSignal) sample() int64 { return s.total }

func TestControllerHalvesOnRateLimitedWindow(t *testing.T) {
	t.Parallel()

	pool := workpool.NewPool(8)
	sig := &stubSignal{}
	c := workpool.NewController(pool, sig.sample, workpool.WithBounds(1, 8))

	sig.total = 3

	c.Step()

	assert.Equal(t, 4, pool.Size(), "any hits in the window halve the pool")

	sig.total = 4

	c.Step()

	assert.Equal(t, 2, pool.Size(), "each rate-limited window halves again")
}

func TestControllerHalvingRespectsFloor(t *testing.T) {
	t.Parallel()

	pool := workpool.NewPool(4)
	sig := &stubSignal{}
	c := workpool.NewController(pool, sig.sample, workpool.WithBounds(2, 8))

	for i := range 4 {
		sig.total = int64(i + 1)

		c.Step()
	}

	assert.Equal(t, 2, pool.Size(), "halving never goes below the floor")
}

func TestControllerSlowStartDoublesUntilThrottled(t *testing.T) {
	t.Parallel()

	// A run that has never been throttled ramps like TCP slow start, so a pool
	// beginning at one worker reaches the ceiling in a few clean windows.
	pool := workpool.NewPool(1)
	sig := &stubSignal{}
	c := workpool.NewController(pool, sig.sample, workpool.WithBounds(1, 16))

	want := []int{2, 4, 8, 16, 16}
	for _, w := range want {
		c.Step()
		assert.Equal(t, w, pool.Size(), "clean windows double up to the ceiling")
	}
}

func TestControllerGrowsAdditivelyAfterThrottle(t *testing.T) {
	t.Parallel()

	pool := workpool.NewPool(8)
	sig := &stubSignal{}
	c := workpool.NewController(pool, sig.sample, workpool.WithBounds(1, 16))

	sig.total = 1

	c.Step()
	assert.Equal(t, 4, pool.Size(), "the throttled window halves the pool")

	c.Step()
	assert.Equal(t, 5, pool.Size(), "recovery after a throttle adds one worker")

	c.Step()
	assert.Equal(t, 6, pool.Size(), "recovery keeps probing one worker at a time")
}

func TestControllerChargesOnlyTheWindowDelta(t *testing.T) {
	t.Parallel()

	pool := workpool.NewPool(4)
	sig := &stubSignal{total: 10}
	c := workpool.NewController(pool, sig.sample, workpool.WithBounds(1, 8))

	// The first step charges everything since construction; the next window is
	// clean because the cumulative total did not move.
	c.Step()
	assert.Equal(t, 2, pool.Size())

	c.Step()
	assert.Equal(t, 3, pool.Size(), "an unmoved cumulative counter reads as a clean window")
}

func TestControllerReportsResizes(t *testing.T) {
	t.Parallel()

	var got []string

	pool := workpool.NewPool(4)
	sig := &stubSignal{}
	c := workpool.NewController(pool, sig.sample,
		workpool.WithBounds(1, 4),
		workpool.WithOnResize(func(prev, next int, hits int64) {
			got = append(got, fmt.Sprintf("%d->%d hits=%d", prev, next, hits))
		}),
	)

	sig.total = 2

	c.Step()

	// At the ceiling with a clean window there is nothing to change, so no
	// callback fires.
	pool.Resize(4)
	c.Step()

	assert.Equal(t, []string{"4->2 hits=2"}, got)
}

func TestControllerBoundsClamp(t *testing.T) {
	t.Parallel()

	pool := workpool.NewPool(1)
	sig := &stubSignal{}

	// A floor below one is raised to one, and a ceiling below the floor is
	// raised to the floor, so the controller can always run.
	c := workpool.NewController(pool, sig.sample, workpool.WithBounds(0, -1))

	c.Step()
	assert.Equal(t, 1, pool.Size(), "clamped bounds pin the pool at one")
}
