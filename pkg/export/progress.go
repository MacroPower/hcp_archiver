package export

// Export phase names, the stage of work a [Progress] hook is being told
// about. Every stage that can run long enough to watch has one, so a view
// naming the phase says what the export is doing even where no total exists
// to size it.
const (
	// PhaseClear covers emptying a non-empty target under [WithForce]. Its
	// size is the target's existing tree, not the archive's, so it carries no
	// total.
	PhaseClear = "clear"
	// PhaseScan covers listing an organization's projects and their
	// workspaces and stacks, totalled by project count.
	PhaseScan = "scan"
	// PhaseExport covers rendering the pages themselves, totalled by
	// renderable unit.
	PhaseExport = "export"
)

// Progress receives the export's progress: the phase under way, the
// organization or item it is working through, the unit total once one is
// known, then one advance per completed unit. Totals and advances always
// belong to the phase last named, which restarts them, so a view can size and
// time each phase on its own. Callers adapt their own progress reporting to
// it, so the export drives live progress views without this package importing
// the progress machinery.
type Progress interface {
	// SetPhase names the stage of work about to run: one of [PhaseClear],
	// [PhaseScan], or [PhaseExport].
	SetPhase(phase string)
	// SetTarget names what the phase is working through: the organization, or
	// the project and workspace under it.
	SetTarget(name string)
	// SetTotal seeds the current phase's denominator. A phase whose size
	// cannot be known ahead of it never calls this, leaving it indeterminate.
	SetTotal(total int)
	// Advance reports n more completed units in the current phase.
	Advance(n int)
}

// nopProgress is the default [Progress]: every report is dropped, so the
// exporter's call sites stay unconditional.
type nopProgress struct{}

func (nopProgress) SetPhase(string)  {}
func (nopProgress) SetTarget(string) {}
func (nopProgress) SetTotal(int)     {}
func (nopProgress) Advance(int)      {}

// WithProgress sets the [Progress] hook the export reports its rendering
// progress through. A nil hook is ignored: the export runs silently, the
// default. It returns an [Option].
func WithProgress(p Progress) Option {
	return func(e *Exporter) {
		if p != nil {
			e.progress = p
		}
	}
}
