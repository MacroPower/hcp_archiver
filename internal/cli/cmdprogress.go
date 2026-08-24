package cli

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/export"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/progress"
	"go.jacobcolvin.com/hcp_archiver/pkg/restore"
)

// cmdProgress carries a command's progress into the reporter's views. It is
// both the [progress.TallySource] the reporter reads (the organization being
// worked through in Target, its completed units in Done, its failures in
// Errored) and the phase hook a command's library drives ([export.Progress]
// for an export, [restore.ProgressSink] for a pull; the inspect commands
// drive it directly), so the views carry live figures for any command the
// same way the ledger's do for an archive. Construction is two-step because
// [progress.New] needs the tally source before the reporter exists, so the
// reporter field is set just after it returns; see [cmdRun.startReporter].
// Safe for
// concurrent use: the reporter's background loop reads the counters while the
// command advances them.
type cmdProgress struct {
	reporter *progress.Reporter
	target   atomic.Pointer[string]
	done     atomic.Int64
	errored  atomic.Int64
}

var (
	_ progress.TallySource = (*cmdProgress)(nil)
	_ export.Progress      = (*cmdProgress)(nil)
	_ restore.ProgressSink = (*cmdProgress)(nil)
)

// Tally returns a point-in-time snapshot of the command's counters.
func (p *cmdProgress) Tally() manifest.Tally {
	var target string

	if t := p.target.Load(); t != nil {
		target = *t
	}

	return manifest.Tally{
		Target:  target,
		Done:    int(p.done.Load()),
		Errored: int(p.errored.Load()),
	}
}

// SetPhase names the work stage the reporter's views label.
func (p *cmdProgress) SetPhase(phase string) {
	p.reporter.SetPhase(phase)
}

// SetTarget records what the phase is working through.
func (p *cmdProgress) SetTarget(name string) {
	p.target.Store(&name)
}

// SetTotal seeds the reporter's phase denominator and resets the done count,
// so both counters read per-phase, the way the archiver's per-org reporters
// do. The errored count deliberately survives: it is the run-wide failure
// figure.
func (p *cmdProgress) SetTotal(total int) {
	p.reporter.SetTotal(total)
	p.done.Store(0)
}

// Advance moves the reporter's phase bar and the done count by n.
func (p *cmdProgress) Advance(n int) {
	p.reporter.Advance(n)
	p.done.Add(int64(n))
}

// Errored counts n more failed units. The count accumulates across the whole
// command rather than resetting per phase, so the views carry every failure
// the run has hit, not just the current phase's.
func (p *cmdProgress) Errored(n int) {
	p.errored.Add(int64(n))
}

// cmdRun carries one command invocation's shared run scaffolding: the parsed
// progress mode and the contexts its work and reporter run under. Create
// instances with [newCmdRun].
type cmdRun struct {
	// The signal-bound context; the reporter's background loop runs under
	// it, so an external SIGINT still erases the panel.
	ctx context.Context
	// The cancelable child the command's work runs under: the terminal UI's
	// raw mode keeps the kernel from raising SIGINT, so ctrl+c arrives
	// through the reporter's interrupt callback, which cancels the work
	// while the reporter itself winds down cleanly.
	runCtx    context.Context
	cancelRun context.CancelFunc
	cc        *cobra.Command
	sink      func() progress.LogSink
	mode      config.ProgressMode
	interval  time.Duration
}

// newCmdRun parses the command's progress mode (before any I/O, so a bad
// value fails without touching the archive) and derives the run's contexts.
// The returned cleanup cancels the run and releases the signal handler;
// defer it.
func newCmdRun(
	cc *cobra.Command, progressFlag string, interval time.Duration, sink func() progress.LogSink,
) (*cmdRun, func(), error) {
	mode, err := config.ParseProgressMode(progressFlag)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // The sentinel-bearing config error renders as-is.
	}

	ctx, stop := signalContext(cc.Context())
	runCtx, cancelRun := context.WithCancel(ctx)

	run := &cmdRun{
		ctx:       ctx,
		runCtx:    runCtx,
		cancelRun: cancelRun,
		cc:        cc,
		sink:      sink,
		mode:      mode,
		interval:  interval,
	}

	return run, func() {
		cancelRun()
		stop()
	}, nil
}

// startReporter builds the run's [cmdProgress] adapter over a
// [progress.Reporter] writing to the command's stderr, names the reporter's
// first phase, and starts the reporter's background loop. The returned stop
// erases the panel and ends the loop; defer it so every error path restores
// the terminal, and call it inline before the first stdout write.
func (r *cmdRun) startReporter(phase string) (*cmdProgress, func()) {
	prog := &cmdProgress{}
	reporter := progress.New(r.cc.ErrOrStderr(), r.mode, prog,
		progress.WithInterval(r.interval),
		progress.WithInterrupt(r.cancelRun),
		progress.WithLogSink(r.sink()),
	)
	prog.reporter = reporter
	reporter.SetPhase(phase)

	return prog, reporter.RunBackground(r.ctx, cmdLogger(r.ctx))
}
