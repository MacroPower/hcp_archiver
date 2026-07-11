package workpool

import (
	"context"
	"fmt"
	"sync"
)

// Pool is a resizable pool of worker slots: a counting semaphore whose
// capacity can change while acquisitions are in flight.
//
// Workers acquire a slot before doing one unit of work and release it after,
// so the pool's size is the number of units in flight at once. [Pool.Resize]
// moves the capacity live: growth admits queued waiters immediately, while a
// shrink drains lazily as in-flight work completes, so running work is never
// preempted. Waiters are admitted in acquisition order, so no worker starves
// while the pool stays busy.
//
// Create instances with [NewPool]. A Pool is safe for concurrent use.
type Pool struct {
	waiters []chan struct{}
	size    int
	inUse   int
	mu      sync.Mutex
}

// NewPool creates a new [Pool] with size worker slots. A size below one is
// raised to one, so the pool can always make progress.
func NewPool(size int) *Pool {
	return &Pool{size: max(1, size)}
}

// Acquire blocks until a worker slot is free or ctx is done, and takes the
// slot. It returns nil once the slot is held; the caller must pair it with
// [Pool.Release]. A cancellation while waiting returns ctx's error wrapped.
func (p *Pool) Acquire(ctx context.Context) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("acquire worker slot: %w", err)
	}

	p.mu.Lock()

	// Take the fast path only when nobody is queued, so a burst of acquirers
	// cannot jump ahead of waiters already in line.
	if p.inUse < p.size && len(p.waiters) == 0 {
		p.inUse++
		p.mu.Unlock()

		return nil
	}

	ready := make(chan struct{})
	p.waiters = append(p.waiters, ready)
	p.mu.Unlock()

	select {
	case <-ready:
		return nil

	case <-ctx.Done():
		p.mu.Lock()

		select {
		case <-ready:
			// The slot was granted in the same instant the wait was abandoned;
			// hand it straight to the next waiter rather than leaking it.
			p.inUse--
			p.wakeLocked()

		default:
			p.dropWaiterLocked(ready)
		}

		p.mu.Unlock()

		return fmt.Errorf("acquire worker slot: %w", ctx.Err())
	}
}

// Release returns a held worker slot to the pool, admitting the next waiter
// when the current size allows it. Releasing a slot that was never acquired is
// a caller bug and panics.
func (p *Pool) Release() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.inUse == 0 {
		panic("workpool: Release without a matching Acquire")
	}

	p.inUse--
	p.wakeLocked()
}

// Resize sets the pool's capacity to n worker slots. A value below one is
// raised to one. Growth admits queued waiters immediately; a shrink takes
// effect as in-flight work releases, never by preempting it.
func (p *Pool) Resize(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.size = max(1, n)
	p.wakeLocked()
}

// Size returns the pool's current capacity in worker slots.
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.size
}

// wakeLocked admits queued waiters in order while free slots remain. The
// caller must hold mu.
func (p *Pool) wakeLocked() {
	for p.inUse < p.size && len(p.waiters) > 0 {
		ready := p.waiters[0]
		p.waiters = p.waiters[1:]
		p.inUse++

		close(ready)
	}
}

// dropWaiterLocked removes an abandoned waiter from the queue. The caller must
// hold mu.
func (p *Pool) dropWaiterLocked(ready chan struct{}) {
	for i, w := range p.waiters {
		if w == ready {
			p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)

			return
		}
	}
}
