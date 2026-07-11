package manifest

// Tally is a point-in-time snapshot of the ledger's live counters.
//
// The progress reporter and the run summary both read it through
// [Ledger.Tally], so the reported numbers and the ledger's own counts share one
// source and cannot drift.
//
// The per-status counts are cumulative: they count every object in the ledger by
// its current status, across runs, so a resumed run starts with the objects a
// prior run already settled rather than from zero. BytesDownloaded stays per-run,
// since it measures this run's download activity.
type Tally struct {
	// Target is the current org, project, or workspace being archived, empty
	// when none is set.
	Target string `json:"target,omitempty"`
	// Done counts objects currently recorded as [StatusDone].
	Done int `json:"done"`
	// AbsentPermanently counts objects currently recorded as
	// [StatusAbsentPermanently].
	AbsentPermanently int `json:"absentPermanently"`
	// Skipped counts objects currently recorded as [StatusSkipped].
	Skipped int `json:"skipped"`
	// Errored counts objects currently recorded as [StatusErrored].
	Errored int `json:"errored"`
	// Forbidden counts objects currently recorded as [StatusForbidden].
	Forbidden int `json:"forbidden"`
	// NotApplicable counts objects currently recorded as [StatusNotApplicable].
	NotApplicable int `json:"notApplicable"`
	// Retried counts this run's in-run retries of transient fetch failures. It
	// tallies retry attempts rather than objects, so one object retried twice
	// counts twice, and stays per-run like BytesDownloaded: a retried fetch that
	// succeeds settles its object done, so retries are activity, not a status.
	Retried int64 `json:"retried"`
	// BytesDownloaded is the total bytes downloaded this run.
	BytesDownloaded int64 `json:"bytesDownloaded"`
	// Resumed reports whether this run continued a manifest that a prior run had
	// already recorded objects into, so the counts above include prior-run work.
	Resumed bool `json:"resumed,omitempty"`
}

// Total returns the number of objects currently recorded across every status.
func (t Tally) Total() int {
	return t.Done + t.AbsentPermanently + t.Skipped + t.Errored + t.Forbidden + t.NotApplicable
}

// Failure is one object still recorded errored or forbidden, with the text of
// its last failure, so a run summary can name what failed rather than only
// count it. Instances are produced by [Ledger.Failures].
type Failure struct {
	// RelPath is the object's archive-relative path and ledger key.
	RelPath string
	// Error is the text of the last recorded failure.
	Error string
	// Status is the failure class, [StatusErrored] or [StatusForbidden].
	Status Status
}
