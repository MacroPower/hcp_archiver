// Package collecttest provides test doubles for the collect package's
// collaboration interfaces.
package collecttest

import (
	"sync"

	"go.jacobcolvin.com/hcp_archiver/collect"
)

// RecordingSyncProgress records the calls a sweep drives through it,
// satisfying [collect.SyncProgress]. The zero value is ready to use. It is
// safe for concurrent use: the sweep's sync workers advance concurrently.
type RecordingSyncProgress struct {
	totals   []int
	advanced int
	mu       sync.Mutex
}

var _ collect.SyncProgress = (*RecordingSyncProgress)(nil)

// SetTotal records one seeded denominator.
func (p *RecordingSyncProgress) SetTotal(total int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.totals = append(p.totals, total)
}

// Advance accumulates the advanced units.
func (p *RecordingSyncProgress) Advance(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.advanced += n
}

// Totals returns each SetTotal argument in call order.
func (p *RecordingSyncProgress) Totals() []int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]int(nil), p.totals...)
}

// Advanced returns the sum of every Advance argument.
func (p *RecordingSyncProgress) Advanced() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.advanced
}
