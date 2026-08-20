package cli

import (
	"sync/atomic"

	"go.jacobcolvin.com/hcp_archiver/pkg/export"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/progress"
)

// exportProgress carries the export run's progress into the reporter's views.
// It is both the [progress.TallySource] the reporter reads (the organization
// being rendered in Target, its rendered units in Done) and the [export.Progress]
// hook the exporter drives (totals and advances forward to the reporter's
// phase bar), so the views carry live figures for an export the same way the
// ledger's do for an archive. Construction is two-step because [progress.New]
// needs the tally source before the reporter exists, so the reporter field is
// set just after it returns. Safe for concurrent use: the reporter's
// background loop reads the counters while the export advances them.
type exportProgress struct {
	reporter *progress.Reporter
	target   atomic.Pointer[string]
	done     atomic.Int64
}

var (
	_ progress.TallySource = (*exportProgress)(nil)
	_ export.Progress      = (*exportProgress)(nil)
)

// Tally returns a point-in-time snapshot of the export's counters.
func (p *exportProgress) Tally() manifest.Tally {
	var target string

	if t := p.target.Load(); t != nil {
		target = *t
	}

	return manifest.Tally{Target: target, Done: int(p.done.Load())}
}

// SetPhase names the export stage the reporter's views label.
func (p *exportProgress) SetPhase(phase string) {
	p.reporter.SetPhase(phase)
}

// SetTarget records what the phase is working through.
func (p *exportProgress) SetTarget(name string) {
	p.target.Store(&name)
}

// SetTotal seeds the reporter's phase denominator and resets the done count,
// so both counters read per-organization, the way the archiver's per-org
// reporters do.
func (p *exportProgress) SetTotal(total int) {
	p.reporter.SetTotal(total)
	p.done.Store(0)
}

// Advance moves the reporter's phase bar and the done count by n.
func (p *exportProgress) Advance(n int) {
	p.reporter.Advance(n)
	p.done.Add(int64(n))
}
