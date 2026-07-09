package manifest

// Status is the recorded outcome of archiving a single object.
//
// Six states drive resume. [StatusDone], [StatusSkipped], and
// [StatusNotApplicable] are settled and never re-requested; permanent absence
// is settled but sticky; [StatusErrored], [StatusForbidden], and any object
// absent from the ledger are retried on the next run.
type Status string

const (
	// StatusDone marks an object that was fetched and written successfully.
	StatusDone Status = "done"
	// StatusAbsentPermanently marks an object gone for good (a 404 or 410); it
	// is settled and sticky, so a re-run does not re-probe it unless recheck is
	// enabled.
	StatusAbsentPermanently Status = "absent-permanently"
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
)

// String returns the on-disk spelling of the status.
func (s Status) String() string {
	return string(s)
}

// Valid reports whether the status is one of the recognized values.
func (s Status) Valid() bool {
	switch s {
	case StatusDone, StatusAbsentPermanently, StatusSkipped, StatusErrored, StatusForbidden, StatusNotApplicable:
		return true
	default:
		return false
	}
}

// Settled reports whether a normal re-run leaves the object alone.
//
// Every status except [StatusErrored] and [StatusForbidden] is settled; an
// object absent from the ledger is never settled. Permanent absence is settled
// but sticky, so a recheck can still force it to be re-probed. Forbidden is not
// settled, so a re-run under a token with broader access retries it.
func (s Status) Settled() bool {
	switch s {
	case StatusDone, StatusAbsentPermanently, StatusSkipped, StatusNotApplicable:
		return true
	case StatusErrored, StatusForbidden:
		return false
	default:
		return false
	}
}
