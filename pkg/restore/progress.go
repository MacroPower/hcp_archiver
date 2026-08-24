package restore

// Restore phase names, the stage of work a [ProgressSink] is being told
// about. Every stage that can run long enough to watch has one, so a view
// naming the phase says what the restore is doing even where no total exists
// to size it.
const (
	// PhaseRestore covers transferring the plan's data files and ledger
	// snapshots, totalled by file.
	PhaseRestore = "restore"
	// PhaseStubs covers backfilling the eviction stubs for the mirror's
	// configuration-version tarballs, totalled by stub.
	PhaseStubs = "stubs"
	// PhaseSettle covers rewriting the marker into its settled form; it
	// carries no total.
	PhaseSettle = "settle"
)

// ProgressSink receives a pull's phase structure: the phase under way, the
// unit total once one is known, then one advance per settled unit. Totals and
// advances always belong to the phase last named, which restarts them, so a
// view can size and time each phase on its own. Callers adapt their own
// progress reporting to it, so the restore drives live progress views without
// this package importing the progress machinery.
type ProgressSink interface {
	// SetPhase names the stage of work about to run: one of [PhaseRestore],
	// [PhaseStubs], or [PhaseSettle].
	SetPhase(phase string)
	// SetTotal seeds the current phase's denominator. A non-positive total
	// marks the phase indeterminate.
	SetTotal(total int)
	// Advance reports n more settled units in the current phase; a unit that
	// failed still settles.
	Advance(n int)
	// Errored reports n more failed transfers. A failed transfer advances
	// and errors both, so the phase completes while the failure count says
	// how much of it went wrong.
	Errored(n int)
}

// nopSink is the default [ProgressSink]: every report is dropped, so the
// restorer's call sites stay unconditional.
type nopSink struct{}

func (nopSink) SetPhase(string) {}
func (nopSink) SetTotal(int)    {}
func (nopSink) Advance(int)     {}
func (nopSink) Errored(int)     {}

// WithProgressSink is an [Option] that sets the [ProgressSink] the restore
// reports its phase structure through. A nil sink is ignored: the restore
// runs without phase reporting, the default. Advances may arrive from
// concurrent transfers.
func WithProgressSink(sink ProgressSink) Option {
	return func(r *Restorer) {
		if sink != nil {
			r.sink = sink
		}
	}
}
