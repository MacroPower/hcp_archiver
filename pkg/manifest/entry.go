package manifest

import (
	"slices"
	"time"
)

// Entry is the per-object record keyed by an archive-relative path.
//
// Instances are produced and updated by the [Ledger] record methods and read
// back as a copy through [Ledger.Entry].
type Entry struct {
	// FirstSeen is when the object first entered the ledger.
	FirstSeen time.Time `json:"firstSeen"`
	// FetchedAt is when the object was last fetched or probed; it is zero for an
	// object that has only ever errored.
	FetchedAt time.Time `json:"fetchedAt,omitzero"`
	// UpdatedAt is the server-side updated-at the object reported when it was
	// last recorded done (see [WithUpdatedAt]); it is zero when the recorder
	// had none. Unlike [Entry.FetchedAt] it reads the server's clock, so it
	// compares against other server timestamps without cross-clock skew.
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	// LastErrorAt is when the last failure was recorded.
	LastErrorAt time.Time `json:"lastErrorAt,omitzero"`
	// Signature is the content fingerprint recorded on a successful fetch.
	Signature *Signature `json:"signature,omitempty"`
	// Status is the current recorded outcome.
	Status Status `json:"status"`
	// LastError is the text of the last failure, empty on success.
	LastError string `json:"lastError,omitempty"`
	// DerivedChildren lists the archive-relative paths of the children a
	// settled listing derived (a log per policy check, an actor per run
	// event), stamped by [Ledger.RecordDerived] after the listing settles. A
	// skip gated on the listing consults it through [Ledger.DerivedSettled],
	// so a child whose own entry was lost re-opens the listing rather than
	// staying a silent, permanent gap.
	DerivedChildren []string `json:"derivedChildren,omitempty"`
	// Attempts counts how many times the object has been recorded.
	Attempts int `json:"attempts"`
	// Transient reports whether the last failure was transient rather than
	// terminal.
	Transient bool `json:"transient,omitempty"`
	// The counted flag tracks whether this entry contributes to the current run
	// tally, so a re-record swaps status counts instead of double-counting.
	counted bool
}

// cloneEntry returns a detached copy of e with its [Signature] and
// [Entry.DerivedChildren] deep-copied, so a later mutation of either the copy
// or the original aliases neither. It is the shared per-entry copy
// [shard.drainDirty], [shard.document], and [Ledger.Entry] make before an
// entry leaves the ledger lock.
func cloneEntry(e Entry) Entry {
	if e.Signature != nil {
		sig := *e.Signature
		e.Signature = &sig
	}

	e.DerivedChildren = slices.Clone(e.DerivedChildren)

	return e
}

// RunRecord summarizes a single archive run.
//
// It is written into the ledger by [Ledger.FinishRun] and returned to the
// caller for the final summary line.
type RunRecord struct {
	// StartedAt is when the run began.
	StartedAt time.Time `json:"startedAt"`
	// FinishedAt is when the run finished.
	FinishedAt time.Time `json:"finishedAt"`
	// Totals holds the per-status count of objects recorded during the run.
	Totals map[Status]int `json:"totals,omitempty"`
	// BytesDownloaded is the total bytes downloaded during the run.
	BytesDownloaded int64 `json:"bytesDownloaded"`
}

// cloneRunRecord returns a detached copy of r with its Totals map copied, so a
// later mutation of either the copy or the original aliases neither. It is the
// shared copy the shard drain, the snapshot document, and the run getters make
// before a run summary leaves the ledger lock.
func cloneRunRecord(r RunRecord) RunRecord {
	r.Totals = copyStatusCounts(r.Totals)

	return r
}
