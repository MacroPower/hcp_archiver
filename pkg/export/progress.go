package export

// Progress receives the export's rendering progress: per organization, the
// name about to render, the unit total once its listings are gathered, then
// one advance per rendered unit (a project index, a workspace, or a stack).
// Callers adapt their own progress reporting to it, so the export drives
// live progress views without this package importing the progress machinery.
type Progress interface {
	// SetTarget names the organization about to render.
	SetTarget(name string)
	// SetTotal seeds the organization's denominator: how many units its
	// render will advance.
	SetTotal(total int)
	// Advance reports n more rendered units.
	Advance(n int)
}

// nopProgress is the default [Progress]: every report is dropped, so the
// exporter's call sites stay unconditional.
type nopProgress struct{}

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
