package manifest

// Status is the recorded outcome of archiving a single object.
//
// Nine states drive resume. [StatusDone], [StatusSkipped],
// [StatusNotApplicable], and [StatusReferenceCleared] are settled and never
// re-requested; [StatusAbsent] and [StatusReferenceAbsent] are settled but
// sticky; [StatusErrored], [StatusForbidden], [StatusPending], and any object
// absent from the ledger are retried on the next run.
type Status string

const (
	// StatusDone marks an object that was fetched and written successfully.
	StatusDone Status = "done"
	// StatusAbsent marks an object the API answered as gone (a 404); it is
	// settled and sticky, so a re-run does not re-probe it unless retry-absent
	// is enabled (see [WithRetryAbsent]).
	StatusAbsent Status = "absent"
	// StatusSkipped marks an object intentionally deferred; it is settled so a
	// re-run does not mistake it for a gap.
	StatusSkipped Status = "skipped"
	// StatusErrored marks a fetch that failed transiently or terminally; it is
	// retried on the next run.
	StatusErrored Status = "errored"
	// StatusForbidden marks a fetch the archiving identity is not permitted to
	// read (an HTTP 403, such as an endpoint that rejects a team or organization
	// token). It is retried on the next run like [StatusErrored], so re-running
	// with a differently scoped token captures a superset of what any single
	// token can see; it is recorded and counted apart from a genuine error so a
	// permission gap is never mistaken for a failure.
	StatusForbidden Status = "forbidden"
	// StatusNotApplicable marks an object that does not apply to this archive
	// (a deferred or low-value surface); it is settled.
	StatusNotApplicable Status = "not-applicable"
	// StatusPending marks a run-scoped reference gate: a ledger-only proxy under a
	// run's own prefix that mirrors whether a cross-shard write the run depends on
	// (its created-by user, an event actor, a config-version tarball) has settled.
	// It is unsettled but not a failure, so [Collection.HasUnsettled] retries the
	// run while the foreign write remains outstanding, yet it never inflates the
	// error tally or the failure list. See [Ledger.MirrorReference].
	StatusPending Status = "pending"
	// StatusReferenceCleared marks a run-scoped reference gate whose cross-shard
	// write has settled: the settled counterpart of [StatusPending]. Like it, the
	// gate is a ledger-only proxy rather than a real object, so it is settled (the
	// walk stops retrying its run) yet excluded from [Ledger.Tally]'s object
	// counters, keeping a recovered reference out of the user-visible
	// not-applicable and total counts. See [Ledger.MirrorReference].
	StatusReferenceCleared Status = "reference-cleared"
	// StatusReferenceAbsent marks a reference gate whose mirrored cross-shard
	// writes all settled, at least one as a confirmed absence. It is settled (a
	// normal re-run stops retrying the gate's run), but it is the trace a
	// retry-absent run re-opens through: the absence itself settled in a
	// foreign shard the run walk never scans, so without this mark in the
	// run's own shard the flag could never reach its target. See
	// [Ledger.MirrorReferenceAbsent].
	StatusReferenceAbsent Status = "reference-absent"
)

// statusProps records, per status, whether a normal re-run leaves it settled
// and whether it is a run-scoped reference gate rather than an archived object.
// Membership lives here once so [Status.Valid], [Status.Settled], and
// [Status.IsGate] cannot drift apart: adding a status is a single row that sets
// both flags, and Valid recognizes exactly the statuses this table names.
var statusProps = map[Status]struct{ settled, gate bool }{
	StatusDone:             {settled: true},
	StatusAbsent:           {settled: true},
	StatusSkipped:          {settled: true},
	StatusNotApplicable:    {settled: true},
	StatusReferenceCleared: {settled: true, gate: true},
	StatusReferenceAbsent:  {settled: true, gate: true},
	StatusErrored:          {},
	StatusForbidden:        {},
	StatusPending:          {gate: true},
}

// String returns the on-disk spelling of the status.
func (s Status) String() string {
	return string(s)
}

// Valid reports whether the status is one of the recognized values.
func (s Status) Valid() bool {
	_, ok := statusProps[s]

	return ok
}

// IsGate reports whether the status marks a run-scoped reference-gate proxy
// ([StatusPending] or [StatusReferenceCleared]) rather than an archived object.
//
// Gate entries drive the walk's retry machinery but represent no file, so
// every surface that counts or reports objects excludes them through this one
// predicate; a gate status added later joins here once and every counting
// surface follows.
func (s Status) IsGate() bool {
	return statusProps[s].gate
}

// Settled reports whether a normal re-run leaves the object alone.
//
// Every status except [StatusErrored], [StatusForbidden], and [StatusPending]
// is settled; an object absent from the ledger is never settled. Absence is
// settled but sticky, so retry-absent can still force it to be re-probed.
// Forbidden is not settled, so a re-run under a token with broader access
// retries it. Pending is not settled, so a reference gate keeps its run in the
// walk's retry set until the foreign write it mirrors settles; once it does,
// [StatusReferenceCleared] settles the gate and the retry stops.
func (s Status) Settled() bool {
	return statusProps[s].settled
}
