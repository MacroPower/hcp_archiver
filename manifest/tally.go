package manifest

// Tally is a point-in-time snapshot of the ledger's live counters.
//
// The progress reporter and the run summary both read it through
// [Ledger.Tally], so the reported numbers and the ledger's own counts share one
// source and cannot drift.
type Tally struct {
	// Target is the current org, project, or workspace being archived, empty
	// when none is set.
	Target string `json:"target,omitempty"`
	// Done counts objects recorded as [StatusDone] this run.
	Done int `json:"done"`
	// AbsentPermanently counts objects recorded as [StatusAbsentPermanently]
	// this run.
	AbsentPermanently int `json:"absentPermanently"`
	// Skipped counts objects recorded as [StatusSkipped] this run.
	Skipped int `json:"skipped"`
	// Errored counts objects recorded as [StatusErrored] this run.
	Errored int `json:"errored"`
	// Forbidden counts objects recorded as [StatusForbidden] this run.
	Forbidden int `json:"forbidden"`
	// NotApplicable counts objects recorded as [StatusNotApplicable] this run.
	NotApplicable int `json:"notApplicable"`
	// BytesDownloaded is the total bytes downloaded this run.
	BytesDownloaded int64 `json:"bytesDownloaded"`
}

// Total returns the number of objects recorded across every status this run.
func (t Tally) Total() int {
	return t.Done + t.AbsentPermanently + t.Skipped + t.Errored + t.Forbidden + t.NotApplicable
}
