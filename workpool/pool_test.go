package workpool_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/workpool"
)

// acquired reports whether an Acquire started in a goroutine completes within
// a generous bound, so tests can observe blocking without flaking.
func acquired(t *testing.T, p *workpool.Pool) chan error {
	t.Helper()

	done := make(chan error, 1)

	go func() {
		done <- p.Acquire(t.Context())
	}()

	return done
}

// settle gives a goroutine that should stay blocked a moment to prove it; a
// sleep cannot prove liveness, only give a would-be bug room to surface.
func settle() {
	time.Sleep(20 * time.Millisecond)
}

func TestPoolAcquireUpToSize(t *testing.T) {
	t.Parallel()

	p := workpool.NewPool(2)

	require.NoError(t, p.Acquire(t.Context()))
	require.NoError(t, p.Acquire(t.Context()))

	// The third acquire must block until a slot is released.
	third := acquired(t, p)

	settle()

	select {
	case <-third:
		t.Fatal("acquire beyond the pool size must block")
	default:
	}

	p.Release()

	select {
	case err := <-third:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a released slot must admit the waiter")
	}
}

func TestPoolNewClampsSizeToOne(t *testing.T) {
	t.Parallel()

	p := workpool.NewPool(0)

	assert.Equal(t, 1, p.Size())
	require.NoError(t, p.Acquire(t.Context()), "a clamped pool still grants its one slot")
}

func TestPoolAcquireCanceledWhileWaiting(t *testing.T) {
	t.Parallel()

	p := workpool.NewPool(1)
	require.NoError(t, p.Acquire(t.Context()))

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() {
		done <- p.Acquire(ctx)
	}()

	settle()
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("a canceled waiter must return")
	}

	// The abandoned wait must not have consumed the slot: releasing the one
	// holder must leave the slot immediately acquirable.
	p.Release()
	require.NoError(t, p.Acquire(t.Context()))
}

func TestPoolAcquireCanceledBeforeWaiting(t *testing.T) {
	t.Parallel()

	p := workpool.NewPool(1)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, p.Acquire(ctx), context.Canceled)
}

func TestPoolResizeGrowAdmitsWaiter(t *testing.T) {
	t.Parallel()

	p := workpool.NewPool(1)
	require.NoError(t, p.Acquire(t.Context()))

	waiting := acquired(t, p)

	settle()

	p.Resize(2)

	select {
	case err := <-waiting:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("growth must admit the queued waiter")
	}

	assert.Equal(t, 2, p.Size())
}

func TestPoolResizeShrinkDrainsLazily(t *testing.T) {
	t.Parallel()

	p := workpool.NewPool(2)
	require.NoError(t, p.Acquire(t.Context()))
	require.NoError(t, p.Acquire(t.Context()))

	p.Resize(1)

	// One release drops in-flight work to the new size; the slot must not be
	// re-granted while the pool is still at capacity.
	p.Release()

	waiting := acquired(t, p)

	settle()

	select {
	case <-waiting:
		t.Fatal("a shrunk pool must not admit waiters while still at capacity")
	default:
	}

	// The second release frees a slot under the new size and admits the waiter.
	p.Release()

	select {
	case err := <-waiting:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("draining to the new size must admit the waiter")
	}
}

func TestPoolReleaseWithoutAcquirePanics(t *testing.T) {
	t.Parallel()

	p := workpool.NewPool(1)

	assert.Panics(t, p.Release)
}

func TestPoolConcurrentAcquireRelease(t *testing.T) {
	t.Parallel()

	// Hammer the pool from many goroutines while it resizes, so the race
	// detector can see any unsynchronized state.
	p := workpool.NewPool(3)

	var wg sync.WaitGroup

	for range 50 {
		wg.Go(func() {
			err := p.Acquire(t.Context())
			if err != nil {
				return
			}

			p.Release()
		})
	}

	p.Resize(1)
	p.Resize(8)

	wg.Wait()
}
