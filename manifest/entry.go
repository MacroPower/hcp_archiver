package manifest

import "time"

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
	// LastErrorAt is when the last failure was recorded.
	LastErrorAt time.Time `json:"lastErrorAt,omitzero"`
	// Signature is the content fingerprint recorded on a successful fetch.
	Signature *Signature `json:"signature,omitempty"`
	// Status is the current recorded outcome.
	Status Status `json:"status"`
	// LastError is the text of the last failure, empty on success.
	LastError string `json:"lastError,omitempty"`
	// Attempts counts how many times the object has been recorded.
	Attempts int `json:"attempts"`
	// Transient reports whether the last failure was transient rather than
	// terminal.
	Transient bool `json:"transient,omitempty"`
	// The counted flag tracks whether this entry contributes to the current run
	// tally, so a re-record swaps status counts instead of double-counting.
	counted bool
}

// cloneEntry returns a detached copy of e with its [Signature] deep-copied, so a
// later mutation of either the copy or the original aliases neither. It is the
// shared per-entry copy [shard.drainDirty], [shard.document], and [Ledger.Entry]
// make before an entry leaves the ledger lock.
func cloneEntry(e Entry) Entry {
	if e.Signature != nil {
		sig := *e.Signature
		e.Signature = &sig
	}

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
