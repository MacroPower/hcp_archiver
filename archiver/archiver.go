package archiver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/progress"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
	"go.jacobcolvin.com/hcp_archiver/workpool"
)

// defaultFlushInterval is the cadence at which each organization's ledger is
// flushed durably to disk while its collectors run.
const defaultFlushInterval = 10 * time.Second

var (
	// ErrRunIncomplete reports that the run finished without archiving every
	// organization: at least one returned a non-cancellation error or recorded
	// only failures with nothing captured. The per-organization and per-object
	// details are logged as the run proceeds; this sentinel lets the command exit
	// non-zero so a scheduled run does not report success over an empty or broken
	// archive.
	ErrRunIncomplete = errors.New("archive run incomplete")

	// ErrNoOrganizations reports that auto-discovery resolved no organizations to
	// archive: the token sees none, whether the account is genuinely empty or the
	// token has been scoped down. Reporting success over a wholly empty archive
	// would defeat the same guarantee as [ErrRunIncomplete], so the run surfaces
	// this instead.
	ErrNoOrganizations = errors.New("no organizations to archive")
)

// Archiver drives a single archive run across one or more organizations.
//
// It is the composition root: from a validated [config.Config] it builds the
// shared client and, per organization, a fresh store, ledger, collection
// environment, and progress reporter, then schedules the domain collectors and
// owns the cross-cutting runtime (the adaptive worker pool, the flush and
// progress tickers, graceful shutdown, and the closing run record). It holds
// no per-object API knowledge; that lives in the collectors it runs.
//
// The worker pool is the run's one parallelism bound: every API request takes
// a slot through the client's gate, so slots flow to whatever work is ready
// (several small workspaces, or many pieces of one large workspace) rather
// than being pinned one-per-workspace. Every run starts at one worker; a
// controller scales the pool between there and the configured ceiling from
// the run's observed rate limiting, so the run finds its own parallelism
// rather than trusting a configured starting point.
//
// Create instances with [New].
type Archiver struct {
	cfg           *config.Config
	client        *tfeclient.Client
	pool          *workpool.Pool
	logger        *slog.Logger
	w             io.Writer
	logSink       progress.LogSink
	cancelRun     context.CancelFunc
	wireBytes     *atomic.Int64
	rateLimited   *atomic.Int64
	flushInterval time.Duration
}

// Option configures an [Archiver] passed to [New].
//
// The available options are:
//   - [WithWriter]
//   - [WithLogger]
//   - [WithLogSink]
//   - [WithFlushInterval]
type Option func(*Archiver)

// WithWriter sets the writer progress and the default logger are rendered to,
// defaulting to [os.Stderr]. Tests capture output by passing a buffer. A nil
// writer keeps the default. It returns an [Option].
func WithWriter(w io.Writer) Option {
	return func(a *Archiver) {
		if w != nil {
			a.w = w
		}
	}
}

// WithLogger sets the structured logger used for run lifecycle and non-fatal
// per-organization errors, overriding the default built over the writer. A nil
// logger keeps the default. It returns an [Option].
func WithLogger(logger *slog.Logger) Option {
	return func(a *Archiver) {
		if logger != nil {
			a.logger = logger
		}
	}
}

// WithLogSink sets the [progress.LogSink] the per-organization reporter routes
// its terminal UI's log output through, so log lines and the live panel share
// one renderer. A nil sink leaves the UI without a sink. It returns an
// [Option].
func WithLogSink(sink progress.LogSink) Option {
	return func(a *Archiver) {
		a.logSink = sink
	}
}

// WithFlushInterval sets the cadence at which the ledger is flushed durably
// while collectors run. A non-positive value keeps the default. It returns an
// [Option].
func WithFlushInterval(d time.Duration) Option {
	return func(a *Archiver) {
		if d > 0 {
			a.flushInterval = d
		}
	}
}

// New creates a new [Archiver] from cfg.
//
// It applies each [Option] in order and, when none supplied a logger, builds a
// text logger over the resolved writer. It performs no I/O and builds no client
// until [Archiver.Run] is called.
func New(cfg *config.Config, opts ...Option) *Archiver {
	a := &Archiver{
		cfg:           cfg,
		w:             os.Stderr,
		wireBytes:     new(atomic.Int64),
		rateLimited:   new(atomic.Int64),
		flushInterval: defaultFlushInterval,
	}

	for _, opt := range opts {
		opt(a)
	}

	if a.logger == nil {
		a.logger = slog.New(slog.NewTextHandler(a.w, nil))
	}

	return a
}

// Run archives every resolved organization in turn.
//
// It validates the configuration, builds the one shared client, resolves the
// organizations (the single named one, or every visible organization), and
// archives each sequentially. The orgs run under a cancelable child of ctx
// whose cancel the reporter's terminal UI holds, so an in-UI ctrl+c aborts the
// whole run exactly as an external SIGINT does. A non-cancellation failure of
// one organization is logged and does not abort the others, but leaves the run
// incomplete; a cancellation aborts the run and is returned wrapped so the
// command can map it to a graceful exit. Run returns nil on clean completion and
// [ErrRunIncomplete] when any organization returned a non-cancellation error or
// captured nothing but failures.
func (a *Archiver) Run(ctx context.Context) error {
	err := a.cfg.Validate()
	if err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	// One pool of worker slots serves the whole run: every request the client
	// makes takes a slot through the gate, so parallelism follows the work
	// rather than a fixed per-workspace assignment. It starts at one worker and
	// leaves the ramp-up to the controller's slow start, and hangs on the
	// archiver so each org's progress reporter can watch its live size.
	a.pool = workpool.NewPool(1)

	// Orgs run sequentially and each reporter windows deltas of the counter, so
	// one shared monotonic wire-byte counter serves the whole run; the
	// rate-limit counter feeds the pool's controller the same way.
	a.client, err = tfeclient.New(
		tfeclient.WithToken(a.cfg.Token),
		tfeclient.WithAddress(a.cfg.Address),
		tfeclient.WithWireBytes(a.wireBytes),
		tfeclient.WithRateLimitCounter(a.rateLimited),
		tfeclient.WithGate(a.pool),
		tfeclient.WithLogger(a.logger),
	)
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	a.cancelRun = cancel

	// Scale the pool from the observed rate limiting for as long as the run
	// lives: double while the server has never pushed back, shed workers the
	// moment it does, and probe back up toward the ceiling while it stays
	// silent. The live worker count and 429 total render in the progress
	// views, so each resize is only a debug event for forensics rather than
	// log noise.
	controller := workpool.NewController(a.pool, a.rateLimited.Load,
		workpool.WithBounds(1, a.cfg.MaxConcurrency),
		workpool.WithOnResize(func(prev, next int, hits int64) {
			a.logger.LogAttrs(runCtx, slog.LevelDebug, "worker_pool_resize",
				slog.Int("from", prev),
				slog.Int("to", next),
				slog.Int64("rate_limited", hits),
			)
		}),
	)

	var controllerWG sync.WaitGroup

	controllerWG.Go(func() {
		controller.Run(runCtx)
	})

	// The controller only returns once runCtx is canceled, so cancel before
	// waiting; deferring the pair keeps the wait after the cancel under LIFO.
	defer func() {
		cancel()
		controllerWG.Wait()
	}()

	orgs, err := a.resolveOrgs(runCtx)
	if err != nil {
		return fmt.Errorf("resolve organizations: %w", err)
	}

	// Auto-discovery found nothing to archive. The loop below would fall straight
	// through and return a clean success over an empty archive, so surface the
	// empty result rather than let a scheduled run report success on it.
	if len(orgs) == 0 {
		return ErrNoOrganizations
	}

	incomplete := false

	for _, orgName := range orgs {
		if runCtx.Err() != nil {
			return fmt.Errorf("archive run: %w", runCtx.Err())
		}

		a.logger.LogAttrs(runCtx, slog.LevelInfo, "org_archive_start",
			slog.String("org", orgName),
		)

		tally, err := a.runOrg(runCtx, orgName)
		if err != nil {
			if runCtx.Err() != nil {
				return fmt.Errorf("archive run: %w", runCtx.Err())
			}

			a.logger.LogAttrs(runCtx, slog.LevelError, "org_archive_error",
				slog.String("org", orgName),
				slog.String("error", err.Error()),
			)

			incomplete = true

			continue
		}

		if orgWhollyFailed(tally) {
			a.logger.LogAttrs(runCtx, slog.LevelError, "org_archive_incomplete",
				slog.String("org", orgName),
				slog.Int("errored", tally.Errored),
				slog.Int("forbidden", tally.Forbidden),
			)

			incomplete = true

			continue
		}

		a.logger.LogAttrs(runCtx, slog.LevelInfo, "org_archive_finish",
			slog.String("org", orgName),
		)
	}

	// Every organization was tried; a non-cancellation failure of one does not
	// abort the rest, but it must not be reported as a clean run either.
	if incomplete {
		return ErrRunIncomplete
	}

	return nil
}
