package manifest

import "time"

// Collection is a handle on one append-mostly collection's ledger state: the
// completion and settled flags, the high-water mark, and the unsettled-child
// gate, all keyed on the collection's archive prefix.
//
// The prefix is the directory the collection's entries live under, so every
// key the handle persists routes to the shard that owns those entries by
// construction: there is no API through which a flag can land in one shard
// while the entries it guards land in another, the split that once let a
// crash freeze entries under a stale settled flag. The prefix is also what
// the unsettled-child gate scans, so the gate and the flags always agree on
// the subtree they describe. Create instances with [Ledger.Collection].
type Collection struct {
	l      *Ledger
	prefix string
}

// Collection returns a [Collection] handle for the collection whose entries
// live under archivePrefix, the archive-relative directory the walk records
// them in.
func (l *Ledger) Collection(archivePrefix string) *Collection {
	return &Collection{l: l, prefix: archivePrefix}
}

// Prefix returns the archive-relative directory the collection's entries live
// under, the key every persisted flag derives from.
func (c *Collection) Prefix() string {
	return c.prefix
}

// Complete reports whether a prior walk reached the collection's true end (or
// the end of its configured history slice), so an interrupted first walk's
// un-archived older tail is never mistaken for settled history.
func (c *Collection) Complete() bool {
	return c.l.isCollectionComplete(c.prefix)
}

// MarkComplete records that a walk reached the collection's end. Completion is
// monotone: nothing un-completes a collection, because the walked history
// itself never un-happens; settledness carries the retry signal instead.
func (c *Collection) MarkComplete() {
	c.l.markCollectionComplete(c.prefix)
}

// Settled reports whether the collection's newest full walk saw only terminal
// elements, the flag that carries a still-running element (recorded done, so
// invisible to a status scan) across passes.
func (c *Collection) Settled() bool {
	return c.l.isCollectionSettled(c.prefix)
}

// SetSettled records whether the newest full walk settled the collection, so a
// later re-run may stop early only while it stays true.
//
// It is deliberately non-monotonic: passing false undoes an earlier true,
// which is how a walk that reaches a still-running element forces the next run
// to re-page the collection until that element settles. A false write is
// additionally drained ahead of every entry record in the next flushed batch
// (see [shard.drainDirty] and [walRecordClass]): unsettlement guards the
// entries a walk is about to record, so it must never become durable after
// them.
func (c *Collection) SetSettled(settled bool) {
	c.l.setCollectionSettled(c.prefix, settled)
}

// HasUnsettled reports whether any recorded entry under the collection's
// prefix is unsettled (errored, forbidden, or a pending reference gate), the
// gate that forces a full re-walk while an unresolved child sits below a done
// boundary.
func (c *Collection) HasUnsettled() bool {
	return c.l.hasUnsettledUnder(c.prefix)
}

// HighWaterMark returns the newest element creation time a walk recorded for
// the collection, or the zero time when none was.
func (c *Collection) HighWaterMark() time.Time {
	return c.l.highWaterMark(c.prefix)
}

// AdvanceHighWaterMark advances the collection's high-water mark toward t,
// keeping the newest value seen; an older t leaves the mark in place.
func (c *Collection) AdvanceHighWaterMark(t time.Time) {
	c.l.advanceHighWaterMark(c.prefix, t)
}
