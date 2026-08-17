package collect

import "time"

// RevisionPath returns which of plainPath or stampedPath a capture of an
// immutable revisioned object should land at, answered from ledger state
// alone.
//
// A revisioned object is replaceable upstream: an upload rewrites the content
// one id downloads and moves its server-side updated-at, while the id stays
// put. The object keeps plainPath until the listing reports an updated-at
// newer than the revision the plain capture recorded, and every revision
// observed after that lands at its own stampedPath, so no revision is
// overwritten by the one that replaced it.
//
// The baseline a listed updated-at is compared against is the server-side
// updated-at the plain capture recorded on its ledger entry (see
// [WithUpdatedAt]), frozen at capture because an immutable entry never
// re-records. Both readings come from the server's clock, so an upload
// landing between a run's download and its ledger stamp, or an archiver clock
// running ahead of the server, cannot make a new revision look already
// captured. An entry recorded before server stamps existed falls back to the
// plain capture's local fetch time, the closest reading it has left. Either
// baseline is stable across runs for an unchanged object, so a revision
// already archived resolves to the same name on every later run and the
// ledger settles its one fetch.
//
// An object whose plain name has not settled keeps it, whether nothing is
// archived there yet or a failed capture is still awaiting its retry, so the
// retry lands on the entry the ledger is waiting for rather than stranding it
// unsettled under a new name. A stamped name a failed capture left unsettled
// is kept the same way. A zero updatedAt is never newer than any baseline, so
// it too keeps the plain name.
func (e *Env) RevisionPath(plainPath, stampedPath string, updatedAt time.Time) string {
	entry, ok := e.Entry(plainPath)
	if !ok || e.ShouldFetch(plainPath) || updatedAt.IsZero() {
		return plainPath
	}

	if _, ok := e.Entry(stampedPath); ok && e.ShouldFetch(stampedPath) {
		return stampedPath
	}

	baseline := entry.UpdatedAt
	if baseline.IsZero() {
		baseline = entry.FetchedAt
	}

	if !updatedAt.After(baseline) {
		return plainPath
	}

	return stampedPath
}
