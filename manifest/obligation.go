package manifest

// Obligation is a persisted marker for work a collection depends on but whose
// outcome no object entry carries: a child enumeration beneath a walk-frozen
// element, or a nested walk whose own settlement is a flag the enclosing
// walk's entry gate cannot see. The marker is a ledger entry only — no file
// ever exists at its key — and its key sits under the collections that must
// stay open, so [Collection.HasUnsettled] finds it.
//
// The shape is acquire-then-settle: [Obligation.Open] persists the intent
// before the work runs, so a crash mid-work leaves the pending marker and the
// next run re-walks down to it, and a forgotten settle costs over-fetching
// rather than a silent permanent gap. [Obligation.Settle] closes the marker
// once the work truly finished; [Obligation.Fail] records the failure so a
// later pass retries it, regressing a previously settled marker freely — the
// marker reflects the newest outcome, never the first. Create instances with
// [Ledger.Obligation].
type Obligation struct {
	l   *Ledger
	key string
}

// Obligation returns the [Obligation] marker at key, which must lie under the
// archive prefix of every collection that must stay open while the obligation
// does.
func (l *Ledger) Obligation(key string) *Obligation {
	return &Obligation{l: l, key: key}
}

// Key returns the marker's ledger key.
func (o *Obligation) Key() string {
	return o.key
}

// Open records the obligation pending, durable intent ahead of the work it
// covers: the pending record drains before any settled record in the same
// flushed batch (see [walRecordClass]), so no crash point persists a skip
// signal for work whose marker was lost. Opening an already-pending or
// already-failed marker changes nothing, so a retry does not churn the entry.
//
// The failed marker survives an [Obligation.Fail] racing the open, too: the
// ledger tests the marker's status and records under one hold of its write
// lock (see [Ledger.openObligation]), so a failure recorded concurrently
// cannot be downgraded to a bare pending marker with its cause and transient
// classification cleared.
func (o *Obligation) Open() {
	o.l.openObligation(o.key)
}

// openObligation records the obligation marker at key [StatusPending] unless
// the marker already stands open: pending, so a retry does not churn it, or
// errored, in which case it is already unsettled and carries its cause, and
// the retry now underway rewrites it through [Obligation.Settle] or
// [Obligation.Fail].
func (l *Ledger) openObligation(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.openGateLocked(key, StatusPending, StatusErrored)
}

// Settle closes the obligation: the work finished and nothing beneath it
// remains outstanding. The settled marker stays out of the object tallies,
// like a cleared reference gate.
func (o *Obligation) Settle() {
	o.l.MirrorReference(o.key, true)
}

// Fail records the obligation's failure with the client's transient
// classification, so the unsettled marker holds every enclosing walk open and
// a later pass retries the work. It regresses a settled marker freely: the
// marker reflects the newest enumeration, and a failure after an earlier
// success is exactly the outcome that must not vanish.
func (o *Obligation) Fail(cause error, transient bool) {
	o.l.RecordErrored(o.key, cause, transient)
}
