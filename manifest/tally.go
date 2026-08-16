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
	// Absent counts objects currently recorded as [StatusAbsent].
	Absent int `json:"absent"`
	// Skipped counts objects currently recorded as [StatusSkipped].
	Skipped int `json:"skipped"`
	// Errored counts objects currently recorded as [StatusErrored].
	Errored int `json:"errored"`
	// Forbidden counts objects currently recorded as [StatusForbidden].
	Forbidden int `json:"forbidden"`
	// NotApplicable counts objects currently recorded as [StatusNotApplicable].
	NotApplicable int `json:"notApplicable"`
	// SurfacesDropped counts the enumeration surfaces dropped this run: listings
	// that failed for a non-cancellation reason, leaving that surface's extent
	// unknown (see [Ledger.MarkSurfaceDropped]). It is per-run, since a re-run
	// retries every enumeration from scratch. A non-zero count marks the run
	// incomplete regardless of the per-object counts, because a dropped listing
	// records no per-object entries for the objects it never named.
	SurfacesDropped int `json:"surfacesDropped,omitempty"`
	// RecordsDiscarded counts the log records the load discarded because their
	// shard's subtree no longer existed on disk: an honored delete-to-re-archive,
	// or unexpected local loss. The ledger cannot tell which, so the count is
	// surfaced in the summary rather than buried in a load-time log line. The
	// discarded objects re-fetch, so a non-zero count is safe, but an operator
	// who deleted nothing should read it as evidence of loss.
	RecordsDiscarded int `json:"recordsDiscarded,omitempty"`
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
	return t.Done + t.Absent + t.Skipped + t.Errored + t.Forbidden + t.NotApplicable
}

// DroppedSurface is one enumeration surface dropped this run, with the text of
// the listing failure, so the run's close can name what was missed rather than
// only count it. Instances are produced by [Ledger.DroppedSurfaces].
type DroppedSurface struct {
	// Surface names the dropped listing (an archive-relative prefix or a
	// collector-scoped label such as "registry/modules").
	Surface string
	// Error is the text of the listing failure.
	Error string
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
